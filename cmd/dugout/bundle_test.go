package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/wire"
)

const (
	bundleUserToken  = "tok-of-user-0123456789abcdef"
	bundleAdminToken = "tok-of-admin-0123456789abcdef"
)

// fakeTools makes a dir with fake git, sw_vers and go, so the bundle never
// runs the real ones. git only answers its version: the doctor's own probe of
// it fails the doctor's probe.
func fakeTools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, script := range map[string]string{
		"git":     "#!/bin/sh\n[ \"$1\" = --version ] && echo 'git version 9.9-fake' && exit 0\n[ \"$1\" = version ] && echo 'git version 9.9-fake' && exit 0\nexit 1\n",
		"sw_vers": "#!/bin/sh\necho 'ProductName: macOS-fake'\n",
		"go":      "#!/bin/sh\necho 'go version go9.9-fake'\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// readTarball returns the files of a .tar.gz by name.
func readTarball(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = string(b)
	}
}

// TestDoctorBundle: doctor --bundle writes a 0600 tarball with the files
// listed, the last 500 lines of the log, no token in any of them, and nothing
// of the state dir's store or bundles. It exits with doctor's own code.
func TestDoctorBundle(t *testing.T) {
	w := newDoctorWorld(t, "dugout@dugout")
	tools := fakeTools(t)
	env := w.with("PATH="+tools+":"+w.path, wire.EnvToken+"="+bundleUserToken, envAdminToken+"="+bundleAdminToken)

	writeService(t, w.home, "/x/dugout", "http://10.0.0.1:7457", tools+":"+w.path)
	logPath := serviceLog(w.home)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i := 1; i <= 600; i++ {
		l := fmt.Sprintf("line %d of the log", i)
		switch i {
		case 300:
			l += " Authorization: Bearer dgt_abc-DEF_123"
		case 301:
			l += " user token " + bundleUserToken
		case 302:
			l += " admin token " + bundleAdminToken
		case 303:
			// The fake tokens are built at run time so secret scanners do not flag the fixture.
			l += " base fetch failed: git fetch https://x-access-token:" + "gh" + "p_URLSECRET123@github.com/o/r.git +refs"
		case 304:
			l += " a bare token " + "gh" + "p_BARESECRET456 and another " + "gl" + "pat-GLSECRET_789 and " + "xo" + "xb-SLACKSECRET-1"
		}
		lines = append(lines, l)
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The state dir is the world's home: files to include, and files never.
	for name, body := range map[string]string{
		"worktrees.json":         `["/a/b","/c/dgt_inworktrees"]`,
		"ratings.json":           `{"rated":[]}`, // an older dugout's, never read
		"epoch":                  "1700000000001\n",
		"daemon_id":              "11111111-1111-4111-8111-111111111111\n",
		"store/HEAD":             "ref: refs/heads/main\n",
		"store/objects/aa/code":  "SECRET-CODE\n",
		"snapshots/ws1.bundle":   "BUNDLE-SECRET\n",
		"bundles/ws2.bundle":     "BUNDLE-SECRET\n",
		"dugout-diag-old.tar.gz": "x",
	} {
		p := filepath.Join(w.home, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out := filepath.Join(t.TempDir(), "diag.tar.gz")
	if err := os.WriteFile(out, []byte("old"), 0o644); err != nil { // a target that exists, with a loose mode
		t.Fatal(err)
	}
	stdout, errOut, code := runIn(t, env, "", "doctor", "--bundle", "-o", out)
	if code != 1 {
		t.Fatalf("exit %d, want doctor's own (1: the binary of the LaunchAgent or unit is not there)\n%s%s", code, stdout, errOut)
	}
	if want := "Diagnostic bundle: " + out + ". Send it to the admin over a private channel.\n"; !strings.HasSuffix(stdout, want) {
		t.Fatalf("stdout does not end with %q:\n%s", want, stdout)
	}
	if !strings.Contains(stdout, "✓ dugout "+version+"\n") {
		t.Errorf("the bundle run lost doctor's output:\n%s", stdout)
	}
	if st, err := os.Stat(out); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("bundle file: %v, %v; want mode 0600", st, err)
	}

	files := readTarball(t, out)
	for _, name := range []string{"doctor.txt", "versions.txt", serviceFile(), "daemon.log",
		"worktrees.json", "epoch", "daemon_id", "state-dir.txt"} {
		if _, ok := files[name]; !ok {
			t.Errorf("no %s in the bundle; it has %v", name, keys(files))
		}
	}
	if len(files) != 8 {
		t.Errorf("the bundle has %v, want exactly the eight files", keys(files))
	}
	if !strings.Contains(files["doctor.txt"], "✓ dugout "+version+"\n") {
		t.Errorf("doctor.txt:\n%s", files["doctor.txt"])
	}
	for _, want := range []string{"dugout " + version, "git version 9.9-fake", "macOS-fake", "go version go9.9-fake"} {
		if !strings.Contains(files["versions.txt"], want) {
			t.Errorf("versions.txt lacks %q:\n%s", want, files["versions.txt"])
		}
	}

	log := files["daemon.log"]
	got := strings.Split(strings.TrimSuffix(log, "\n"), "\n")
	if len(got) != 500 || got[0] != "line 101 of the log" || !strings.HasPrefix(got[499], "line 600 of the log") {
		t.Errorf("daemon.log has %d lines from %q to %q, want the last 500", len(got), got[0], got[len(got)-1])
	}
	if !strings.Contains(log, "line 300 of the log Authorization: Bearer [token]\n") {
		t.Errorf("the dgt_ token of the log was not scrubbed")
	}
	for name, body := range files {
		for _, secret := range []string{"dgt_", bundleUserToken, bundleAdminToken, "SECRET", "URLSECRET", "x-access-token", "BARESECRET", "GLSECRET", "SLACKSECRET"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s contains %q", name, secret)
			}
		}
	}
	if !strings.Contains(log, "git fetch https://[token]@github.com/o/r.git +refs\n") ||
		!strings.Contains(log, "a bare token [token] and another [token] and [token]\n") {
		t.Errorf("the URL userinfo and the bare tokens of the log were not scrubbed:\n%s", log)
	}
	if !strings.Contains(files["worktrees.json"], "[token]") || !strings.Contains(log, "user token [token]") ||
		!strings.Contains(log, "admin token [token]") {
		t.Errorf("the tokens were not replaced by [token]: worktrees.json %q", files["worktrees.json"])
	}

	// The LaunchAgent or the unit never holds a token.
	if p := files[serviceFile()]; !(strings.Contains(p, "ProgramArguments") || strings.Contains(p, "ExecStart=")) || strings.Contains(strings.ToLower(p), "token") {
		t.Errorf("%s:\n%s", serviceFile(), p)
	}
	// The listing names what the state dir holds, with sizes, and no more.
	list := files["state-dir.txt"]
	if !strings.Contains(list, "epoch\t14\n") || !strings.Contains(list, "store/\n") ||
		strings.Contains(list, "HEAD") || strings.Contains(list, "ws1.bundle") || strings.Contains(list, "code") {
		t.Errorf("state-dir.txt:\n%s", list)
	}
}

// TestDoctorBundleMissingFiles: what is not there is skipped and said in
// doctor.txt; the default is dugout-diag-<date>-<time>.tar.gz in $HOME, not in
// the current directory; doctor's exit code is kept, and a failed write is an
// error with exit 1.
func TestDoctorBundleMissingFiles(t *testing.T) {
	w := newDoctorWorld(t, "dugout@dugout")
	env := w.with("PATH=" + fakeTools(t) + ":" + w.path)
	cwd := t.TempDir()
	cmd := exec.Command(bin, "doctor", "--bundle")
	cmd.Env, cmd.Dir = env, cwd
	outb, _ := cmd.Output()
	m := regexp.MustCompile(`Diagnostic bundle: (` + regexp.QuoteMeta(w.home) + `/dugout-diag-\d{8}-\d{6}\.tar\.gz)\. Send it to the admin over a private channel\.\n$`).FindStringSubmatch(string(outb))
	if m == nil {
		t.Fatalf("stdout:\n%s", outb)
	}
	if ents, _ := filepath.Glob(filepath.Join(cwd, "dugout-diag-*")); len(ents) != 0 {
		t.Errorf("the bundle was written in the current directory: %v", ents)
	}
	files := readTarball(t, m[1])
	for _, name := range []string{serviceFile(), "daemon.log", "worktrees.json"} {
		if _, ok := files[name]; ok {
			t.Errorf("%s is in the bundle but was not there", name)
		}
		if !strings.Contains(files["doctor.txt"], "– did not include "+name+": it does not exist\n") {
			t.Errorf("doctor.txt does not note the missing %s:\n%s", name, files["doctor.txt"])
		}
	}

	stdout, errOut, code := runIn(t, env, "", "doctor", "--bundle", "-o", filepath.Join(cwd, "no", "such", "d.tar.gz"))
	if code != 1 || errOut == "" || strings.Contains(stdout, "Diagnostic bundle") {
		t.Errorf("a bundle that cannot be written: exit %d, stdout %q, stderr %q", code, stdout, errOut)
	}
	if _, errOut, code := runIn(t, env, "", "doctor", "-o", "x.tar.gz"); code != 2 || !strings.Contains(errOut, "--bundle") {
		t.Errorf("-o without --bundle: exit %d, stderr %q", code, errOut)
	}
}

// TestDoctorBundleWritesSafely: the bundle is written to a temp file and
// renamed, so a symlink at the target is replaced and not followed, and a
// write that fails leaves what was at the target.
func TestDoctorBundleWritesSafely(t *testing.T) {
	w := newDoctorWorld(t, "dugout@dugout")
	env := w.with("PATH=" + fakeTools(t) + ":" + w.path)
	dir := t.TempDir()

	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("intact"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.tar.gz")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runIn(t, env, "", "doctor", "--bundle", "-o", link); code != 1 && code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if b, _ := os.ReadFile(victim); string(b) != "intact" {
		t.Errorf("the symlink was followed: the file it pointed to now holds %q", b)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0o600 {
		t.Errorf("the target is %v, %v; want a regular file of mode 0600", st, err)
	}
	if _, ok := readTarball(t, link)["doctor.txt"]; !ok {
		t.Error("the replaced target is not the bundle")
	}

	// A target that cannot be replaced (a non-empty directory) stays as it was.
	taken := filepath.Join(dir, "taken")
	if err := os.MkdirAll(filepath.Join(taken, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runIn(t, env, "", "doctor", "--bundle", "-o", taken); code != 1 || errOut == "" {
		t.Errorf("an unwritable target: exit %d, stderr %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(taken, "inside")); err != nil {
		t.Errorf("a failed write damaged what was at the target: %v", err)
	}
	if ents, _ := filepath.Glob(filepath.Join(dir, ".dugout-diag-*")); len(ents) != 0 {
		t.Errorf("temp files left behind: %v", ents)
	}
}

// TestDoctorBundleSkipsNonRegular: a symlink where a file of the state dir
// should be (worktrees.json pointing outside) is not followed, and doctor.txt
// says so.
func TestDoctorBundleSkipsNonRegular(t *testing.T) {
	w := newDoctorWorld(t, "dugout@dugout")
	env := w.with("PATH=" + fakeTools(t) + ":" + w.path)
	outside := filepath.Join(t.TempDir(), "foreign")
	if err := os.WriteFile(outside, []byte("FOREIGN-CONTENT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(w.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(w.home, "worktrees.json")); err != nil {
		t.Fatal(err)
	}
	logPath := serviceLog(w.home)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, logPath); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "diag.tar.gz")
	runIn(t, env, "", "doctor", "--bundle", "-o", out)
	files := readTarball(t, out)
	for name, body := range files {
		if strings.Contains(body, "FOREIGN") {
			t.Errorf("%s holds what the symlink pointed to", name)
		}
	}
	for _, name := range []string{"worktrees.json", "daemon.log"} {
		if _, ok := files[name]; ok {
			t.Errorf("%s is in the bundle, a symlink", name)
		}
		if !strings.Contains(files["doctor.txt"], "– did not include "+name+": not a regular file\n") {
			t.Errorf("doctor.txt does not note %s:\n%s", name, files["doctor.txt"])
		}
	}
}

func keys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
