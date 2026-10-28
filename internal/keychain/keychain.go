// Package keychain keeps a person's Dugout token, one per server URL: in the
// macOS Keychain through the security(1) tool, and elsewhere in the Secret
// Service (GNOME Keyring, KWallet) through libsecret's secret-tool, or, on a
// machine without it, in a 0600 file in the state dir.
//
// The file is the trade-off for headless and minimal Linux boxes, which have
// no Secret Service, and it is what gh, docker and kubectl do there: the token
// sits unencrypted on disk, readable by the person's user and root, and it
// goes wherever the home is backed up. A keyring encrypts it at rest and
// unlocks it with the login. Which one a machine uses is fixed by whether
// secret-tool is on the PATH, so login and the daemon always agree.
package keychain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/statedir"
)

const service = "dugout"

// notFoundCode is security's exit status for a missing item.
const notFoundCode = 44

// timeout bounds security and secret-tool: a locked keychain can block on a
// prompt nobody sees (under launchd or systemd, for one).
var timeout = 10 * time.Second // a var so a test can shorten it

var errTimeout = errors.New("the keychain did not respond in 10 s")

// ErrNotFound reports that the keychain holds no token for the server.
var ErrNotFound = errors.New("no token in the keychain")

// goos and lookPath are vars so a test takes the Linux paths on a Mac and
// never reaches the real secret-tool.
var (
	goos     = runtime.GOOS
	lookPath = exec.LookPath
)

// FileName is the token file's name in the state dir, where secret-tool is
// missing.
const FileName = "tokens.json"

// backend is where the tokens live on this machine: "security", the path of
// secret-tool, or "" for the file.
func backend() string {
	if goos == "darwin" {
		return "security"
	}
	if p, err := lookPath("secret-tool"); err == nil {
		return p
	}
	return ""
}

// Where says where Set keeps the token, for login to tell the person: "the
// keychain", or the token file's path.
func Where() string {
	if backend() != "" {
		return "the keychain"
	}
	path, err := filePath()
	if err != nil {
		return FileName
	}
	return path
}

// Get returns the token stored for server, without the trailing newline.
func Get(server string) (string, error) {
	switch b := backend(); b {
	case "security":
		return run([]string{"security", "find-generic-password", "-s", service, "-a", server, "-w"}, "", isSecurityNotFound)
	case "":
		return fileGet(server)
	default:
		return run([]string{b, "lookup", "service", service, "account", server}, "", isSecretToolNotFound)
	}
}

// Set stores token for server, replacing an earlier one. For security the
// token goes as an argument, which is its contract; secret-tool reads it from
// stdin. Errors never carry it.
func Set(server, token string) error {
	var err error
	switch b := backend(); b {
	case "security":
		_, err = run([]string{"security", "add-generic-password", "-U", "-s", service, "-a", server, "-w", token}, "", nil)
	case "":
		err = fileSet(server, token)
	default:
		// The label is what a keyring app shows; stdin is the secret, as is.
		_, err = run([]string{b, "store", "--label=Dugout " + server, "service", service, "account", server}, token, nil)
	}
	return err
}

func isSecurityNotFound(code int, _ string) bool { return code == notFoundCode }

// isSecretToolNotFound: secret-tool lookup exits 1 both for a missing item
// and for a failure, which only the latter explains on stderr.
func isSecretToolNotFound(code int, stderr string) bool { return code == 1 && stderr == "" }

// run runs argv with stdin, bounded by timeout, and returns its stdout less
// one trailing newline. notFound tells a missing item from a failure by exit
// status and stderr. The error names the tool and its first argument, never
// the rest: that is where security takes the token.
func run(argv []string, stdin string, notFound func(code int, stderr string) bool) (string, error) {
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", errTimeout
		}
		stderr := strings.TrimSpace(errb.String())
		var ee *exec.ExitError
		if notFound != nil && errors.As(err, &ee) && notFound(ee.ExitCode(), stderr) {
			return "", ErrNotFound
		}
		// err is an exit status or a lookup failure: neither holds the argv.
		return "", fmt.Errorf("%s %s: %w: %s", filepath.Base(argv[0]), argv[1], err, stderr)
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

func filePath() (string, error) {
	dir, err := statedir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// tokenFile is the token file: server URL to token.
type tokenFile struct {
	Tokens map[string]string `json:"tokens"`
}

func readFile(path string) (tokenFile, error) {
	f := tokenFile{Tokens: map[string]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("%s unreadable: %w", path, err) // json's errors quote no value
	}
	if f.Tokens == nil {
		f.Tokens = map[string]string{}
	}
	return f, nil
}

func fileGet(server string) (string, error) {
	path, err := filePath()
	if err != nil {
		return "", err
	}
	f, err := readFile(path)
	if err != nil {
		return "", err
	}
	t, ok := f.Tokens[server]
	if !ok {
		return "", ErrNotFound
	}
	return t, nil
}

// fileSet rewrites the file whole with mode 0600 (statedir.WriteAtomic), in a
// state dir it makes 0700, so a daemon reading it meanwhile sees the old file
// or the new one.
func fileSet(server, token string) error {
	path, err := filePath()
	if err != nil {
		return err
	}
	if err := statedir.Ensure(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := readFile(path)
	if err != nil {
		return err
	}
	f.Tokens[server] = token
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return statedir.WriteAtomic(path, append(b, '\n'))
}
