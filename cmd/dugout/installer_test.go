package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRelease serves a release laid out as the release workflow publishes
// it: /releases/latest redirecting to the tag, and the tarball for this
// machine and SHA256SUMS under /releases/download/vX.Y.Z/. The tarball holds
// dugout_X.Y.Z_os_arch/dugout with exe's bytes; sums overrides SHA256SUMS
// when not "".
func fakeRelease(t *testing.T, ver string, exe []byte, sums string) string {
	t.Helper()
	name := fmt.Sprintf("dugout_%s_%s_%s", ver, runtime.GOOS, runtime.GOARCH)
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		body []byte
		mode int64
	}{{name + "/dugout", exe, 0o755}, {name + "/LICENSE", []byte("MIT\n"), 0o644}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.body)
	}
	tw.Close()
	gz.Close()
	if sums == "" {
		sum := sha256.Sum256(tgz.Bytes())
		// sha256sum ./*.tar.gz, as the workflow runs it, with another asset.
		sums = hex.EncodeToString(sum[:]) + "  ./" + name + ".tar.gz\n" + strings.Repeat("0", 64) + "  ./dugout_" + ver + "_other.tar.gz\n"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v"+ver, http.StatusFound)
	})
	mux.HandleFunc("GET /releases/tag/v"+ver, func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /releases/download/v"+ver+"/"+name+".tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(tgz.Bytes()) })
	mux.HandleFunc("GET /releases/download/v"+ver+"/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sums)) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/releases"
}

// runInstaller runs install.sh under sh with the script on stdin, as curl |
// sh -s -- args does, and only env.
func runInstaller(t *testing.T, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	script, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{"-s", "--"}, args...)...)
	cmd.Env, cmd.Stdin = env, bytes.NewReader(script)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

const echoDugout = "#!/bin/sh\necho \"ran $*\"\n[ -t 0 ] && echo 'stdin is a terminal'\nexit 0\n"

// TestInstallerLatest: the installer finds the latest release, checks it,
// installs the real binary in ~/.local/bin and runs it with the arguments.
func TestInstallerLatest(t *testing.T) {
	exe, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	env := []string{"HOME=" + home, "PATH=/bin:/usr/bin", "DUGOUT_RELEASES=" + fakeRelease(t, version, exe, "")}
	out, errOut, code := runInstaller(t, env, "version")
	if code != 0 || out != "dugout "+version+"\n" {
		t.Fatalf("installer: exit %d\n%s\n%s", code, out, errOut)
	}
	got := filepath.Join(home, ".local", "bin", "dugout")
	if st, err := os.Stat(got); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("installed binary: %v %v", st, err)
	}
	if !strings.Contains(errOut, "Downloading dugout "+version+" for "+runtime.GOOS+"/"+runtime.GOARCH) ||
		!strings.Contains(errOut, "Add "+filepath.Dir(got)+" to your PATH") {
		t.Fatalf("installer stderr:\n%s", errOut)
	}
}

// TestInstallerRunsSetupOrArgs: with no arguments it runs dugout setup, with
// some it runs them (an invite's join), and without a terminal setup gets
// none, so it asks nothing.
func TestInstallerRunsSetupOrArgs(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	env := []string{"HOME=" + home, "PATH=" + dir + ":/bin:/usr/bin", "DUGOUT_RELEASES=" + fakeRelease(t, "9.8.7", []byte(echoDugout), ""),
		"DUGOUT_VERSION=v9.8.7", "DUGOUT_BIN_DIR=" + dir}
	if out, errOut, code := runInstaller(t, env); code != 0 || out != "ran setup\n" || strings.Contains(errOut, "to your PATH") {
		t.Fatalf("no arguments: exit %d %q %q", code, out, errOut)
	}
	link := "https://dugout.example#dgi_" + strings.Repeat("a", 43)
	if out, errOut, code := runInstaller(t, env, "join", link); code != 0 || out != "ran join "+link+"\n" {
		t.Fatalf("join: exit %d %q %q", code, out, errOut)
	}
}

// TestInstallerChecksum: a tarball that does not match SHA256SUMS, or one
// SHA256SUMS does not list, installs nothing.
func TestInstallerChecksum(t *testing.T) {
	for _, sums := range []string{
		strings.Repeat("ab", 32) + "  ./dugout_1.2.3_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz\n",
		strings.Repeat("ab", 32) + "  ./dugout_1.2.3_plan9_mips.tar.gz\n",
	} {
		home := t.TempDir()
		env := []string{"HOME=" + home, "PATH=/bin:/usr/bin", "DUGOUT_RELEASES=" + fakeRelease(t, "1.2.3", []byte(echoDugout), sums), "DUGOUT_VERSION=1.2.3"}
		out, errOut, code := runInstaller(t, env)
		if code != 1 || out != "" || (!strings.Contains(errOut, "does not match SHA256SUMS: nothing was installed") && !strings.Contains(errOut, "SHA256SUMS has no line for")) {
			t.Fatalf("bad checksum: exit %d %q %q", code, out, errOut)
		}
		if _, err := os.Stat(filepath.Join(home, ".local", "bin", "dugout")); !os.IsNotExist(err) {
			t.Fatalf("a binary was installed: %v", err)
		}
	}
}

// TestInstallerNoRelease: a version that is not published fails with where
// it looked.
func TestInstallerNoRelease(t *testing.T) {
	env := []string{"HOME=" + t.TempDir(), "PATH=/bin:/usr/bin", "DUGOUT_RELEASES=" + fakeRelease(t, "1.2.3", []byte(echoDugout), ""), "DUGOUT_VERSION=4.5.6"}
	if out, errOut, code := runInstaller(t, env); code != 1 || out != "" || !strings.Contains(errOut, "could not download dugout_4.5.6_") {
		t.Fatalf("missing release: exit %d %q %q", code, out, errOut)
	}
}
