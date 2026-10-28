package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/launchd"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/systemd"
	"github.com/joacofg/dugout/internal/wire"
)

const (
	bundleLogLines = 500
	bundleMaxRead  = 4 << 20 // bytes read of any one file
	minSecretLen   = 8       // an env value shorter than this is no token, and would mangle the text
)

// tokenPattern is every token serve issues, the admin's included
// (serve.NewToken): "dgt_" and base64url.
var tokenPattern = regexp.MustCompile(`dgt_[A-Za-z0-9_-]+`)

// userinfoPattern is the credentials of a URL (scheme://user:pass@host): a
// failed git fetch logs its remote, which may carry a token.
var userinfoPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s@]+@`)

// knownTokenPattern is the well-known prefixes of other services' tokens
// (GitHub, GitLab, Slack) that may sit bare in a log.
var knownTokenPattern = regexp.MustCompile(`(?:ghp_|gho_|ghu_|ghs_|ghr_|github_pat_|glpat-|xoxb-|xoxp-)[A-Za-z0-9_-]+`)

// scrub replaces the tokens in text with [token]: anything shaped like one,
// the userinfo of URLs, and the values of the token variables of this
// environment.
func scrub(text []byte) []byte {
	for _, env := range []string{wire.EnvToken, envAdminToken} {
		if v := os.Getenv(env); len(v) >= minSecretLen {
			text = bytes.ReplaceAll(text, []byte(v), []byte("[token]"))
		}
	}
	text = userinfoPattern.ReplaceAll(text, []byte("${1}[token]@"))
	text = knownTokenPattern.ReplaceAll(text, []byte("[token]"))
	return tokenPattern.ReplaceAll(text, []byte("[token]"))
}

// errNotRegular is a bundle source that is not a plain file: a symlink could
// point anywhere, so it is never followed.
var errNotRegular = errors.New("not a regular file")

// openRegular opens path if it is a regular file, not a symlink to one.
func openRegular(path string) (*os.File, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errNotRegular
	}
	return os.Open(path)
}

// bundleFile is one file of the bundle.
type bundleFile struct {
	name string
	data []byte
}

// writeBundle writes the diagnostic bundle of this machine, with doctorOut as
// doctor.txt, to path, or to dugout-diag-<date>-<time>.tar.gz in $HOME when
// path is empty. It returns where it wrote, with mode 0600: the bundle is
// written to a 0600 temp file next to path and renamed over it, so an
// existing target never holds it under an old mode, a symlink at path is
// replaced and not followed, and a failed write leaves what was there.
//
// What goes in is listed here and nowhere else: doctor's output, versions,
// the LaunchAgent or the systemd unit (neither holds a token), the tail of
// the daemon's log, and a few small files of the state dir. Never its git
// store, its snapshots or bundles (code), nor the keychain or the token file.
// Every file is scrubbed of tokens.
func writeBundle(path string, doctorOut []byte, now time.Time) (string, error) {
	home, homeErr := os.UserHomeDir()
	if path == "" {
		if homeErr != nil {
			return "", fmt.Errorf("could not locate your HOME (use -o <path>): %w", homeErr)
		}
		path = filepath.Join(home, "dugout-diag-"+now.Format("20060102-150405")+".tar.gz")
	}
	var notes bytes.Buffer
	files := []bundleFile{{name: "versions.txt", data: versions()}}
	add := func(name string, read func() ([]byte, error)) {
		b, err := read()
		switch {
		case errors.Is(err, fs.ErrNotExist):
			fmt.Fprintf(&notes, "– did not include %s: it does not exist\n", name)
		case errors.Is(err, errNotRegular):
			fmt.Fprintf(&notes, "– did not include %s: %v\n", name, err)
		case err != nil:
			fmt.Fprintf(&notes, "– did not include %s: %v\n", name, err)
		default:
			files = append(files, bundleFile{name: name, data: b})
		}
	}
	if homeErr != nil {
		fmt.Fprintf(&notes, "– could not locate your HOME: %v\n", homeErr)
	} else {
		if goos == "linux" {
			add(systemd.Unit, func() ([]byte, error) { return readCapped(systemd.UnitPath(home, os.Getenv)) })
		} else {
			add("LaunchAgent.plist", func() ([]byte, error) { return readCapped(launchd.PlistPath(home)) })
		}
		add("daemon.log", func() ([]byte, error) { return tailLines(daemonLog(home), bundleLogLines) })
	}
	dir, err := statedir.Dir()
	if err != nil {
		fmt.Fprintf(&notes, "– could not locate the state directory: %v\n", err)
	} else {
		for _, name := range []string{"worktrees.json", "epoch", "daemon_id"} {
			add(name, func() ([]byte, error) { return readCapped(filepath.Join(dir, name)) })
		}
		add("state-dir.txt", func() ([]byte, error) { return listDir(dir) })
	}
	files = append([]bundleFile{{name: "doctor.txt", data: append(append([]byte{}, doctorOut...), notes.Bytes()...)}}, files...)

	f, err := os.CreateTemp(filepath.Dir(path), ".dugout-diag-*")
	if err != nil {
		return "", err
	}
	if err := writeTarball(f, files, now); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return path, nil
}

func writeTarball(w io.Writer, files []bundleFile, now time.Time) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		data := scrub(f.data)
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(data)), ModTime: now}); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// versions is what runs here: dugout, git, and sw_vers and go when present.
func versions() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "dugout %s (protocol %d)\n", version, wire.ProtocolVersion)
	for _, c := range [][]string{{"git", "--version"}, {"sw_vers"}, {"go", "version"}} {
		path, err := exec.LookPath(c[0])
		if err != nil {
			if c[0] == "git" {
				b.WriteString("git: not found on the PATH\n")
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
		out, err := exec.CommandContext(ctx, path, c[1:]...).CombinedOutput()
		cancel()
		if err != nil {
			fmt.Fprintf(&b, "%s: failed: %v\n", c[0], err)
			continue
		}
		b.Write(bytes.TrimSpace(out))
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func readCapped(path string) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, bundleMaxRead))
}

// tailLines is the last n lines of the file at path, from at most its last
// bundleMaxRead bytes.
func tailLines(path string, n int) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	cut := st.Size() > bundleMaxRead
	if cut {
		if _, err := f.Seek(-bundleMaxRead, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if cut && len(lines) > 0 {
		lines = lines[1:] // the first one starts before what was read
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return []byte(strings.Join(lines, "")), nil
}

// listDir names what dir holds, with the size of each file: names and sizes
// only, and no look inside a directory.
func listDir(dir string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for _, e := range entries {
		if e.IsDir() {
			fmt.Fprintf(&b, "%s/\n", e.Name())
			continue
		}
		if info, err := e.Info(); err == nil {
			fmt.Fprintf(&b, "%s\t%d\n", e.Name(), info.Size())
		}
	}
	return b.Bytes(), nil
}
