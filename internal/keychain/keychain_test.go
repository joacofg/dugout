package keychain

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/statedir"
)

// onPlatform makes the package take platform p's backend for the test.
func onPlatform(t *testing.T, p string) {
	t.Helper()
	old := goos
	goos = p
	t.Cleanup(func() { goos = old })
}

// withSecretTool makes the Linux backend find secret-tool at path, or none
// when path is "": lookPath never searches the real PATH.
func withSecretTool(t *testing.T, path string) {
	t.Helper()
	onPlatform(t, "linux")
	old := lookPath
	lookPath = func(name string) (string, error) {
		if name != "secret-tool" || path == "" {
			return "", exec.ErrNotFound
		}
		return path, nil
	}
	t.Cleanup(func() { lookPath = old })
}

// fakeSecurity puts a security(1) on PATH that stores to a file and exits 44
// for a missing item, so the real Keychain is never touched.
func fakeSecurity(t *testing.T) {
	t.Helper()
	onPlatform(t, "darwin")
	dir := t.TempDir()
	store := filepath.Join(dir, "store")
	script := `#!/bin/sh
case "$1" in
add-generic-password) [ "$2 $3 $4 $5 $7" = "-U -s dugout -a -w" ] || exit 2 ;;
find-generic-password) [ "$2 $3 $4 $6" = "-s dugout -a -w" ] || exit 2 ;;
esac
STORE=` + store + `
case "$1" in
add-generic-password)
  touch "$STORE"
  grep -v "^$6	" "$STORE" > "$STORE.new"
  printf '%s\t%s\n' "$6" "$8" >> "$STORE.new"
  mv "$STORE.new" "$STORE" ;;
find-generic-password)
  [ -f "$STORE" ] && awk -F'\t' -v a="$5" '$1==a{print $2; f=1} END{exit f?0:1}' "$STORE" && exit 0
  exit 44 ;;
esac
`
	writeScript(t, filepath.Join(dir, "security"), script)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// fakeSecretTool is a secret-tool that stores to a file, takes the secret on
// stdin like the real one, and exits 1 with nothing on stderr for a missing
// item. It returns the log of its arguments.
func fakeSecretTool(t *testing.T) (argsLog string) {
	t.Helper()
	dir := t.TempDir()
	store, argsLog := filepath.Join(dir, "store"), filepath.Join(dir, "args.log")
	script := `#!/bin/sh
echo "$*" >>` + argsLog + `
STORE=` + store + `
case "$1" in
store)
  [ "$2 $3 $4 $5" = "--label=Dugout $6 service dugout account" ] || exit 2
  tok=$(cat)
  touch "$STORE"
  grep -v "^$6	" "$STORE" > "$STORE.new"
  printf '%s\t%s\n' "$6" "$tok" >> "$STORE.new"
  mv "$STORE.new" "$STORE" ;;
lookup)
  [ "$2 $3 $4" = "service dugout account" ] || exit 2
  [ -f "$STORE" ] && awk -F'\t' -v a="$5" '$1==a{printf "%s", $2; f=1} END{exit f?0:1}' "$STORE" && exit 0
  exit 1 ;;
*) exit 2 ;;
esac
`
	writeScript(t, filepath.Join(dir, "secret-tool"), script)
	withSecretTool(t, filepath.Join(dir, "secret-tool"))
	return argsLog
}

// fileBackend is a Linux machine without secret-tool, whose state dir is a
// temp DUGOUT_HOME.
func fileBackend(t *testing.T) (stateDir string) {
	t.Helper()
	withSecretTool(t, "")
	stateDir = filepath.Join(t.TempDir(), "state")
	t.Setenv(statedir.EnvHome, stateDir)
	return stateDir
}

func writeScript(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

var backends = map[string]func(t *testing.T){
	"security":    fakeSecurity,
	"secret-tool": func(t *testing.T) { fakeSecretTool(t) },
	"file":        func(t *testing.T) { fileBackend(t) },
}

func TestKeychainGetSet(t *testing.T) {
	for name, setup := range backends {
		t.Run(name, func(t *testing.T) {
			setup(t)
			const a, b = "http://10.0.0.1:7457", "https://office.example"
			if _, err := Get(a); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get before Set = %v, want ErrNotFound", err)
			}
			if err := Set(a, "dgt_one"); err != nil {
				t.Fatal(err)
			}
			if err := Set(b, "dgt_other"); err != nil {
				t.Fatal(err)
			}
			if got, err := Get(a); err != nil || got != "dgt_one" {
				t.Fatalf("Get(a) = %q, %v", got, err)
			}
			if err := Set(a, "dgt_two"); err != nil {
				t.Fatal(err)
			}
			if got, _ := Get(a); got != "dgt_two" {
				t.Fatalf("Get(a) after update = %q", got)
			}
			if got, _ := Get(b); got != "dgt_other" {
				t.Fatalf("Get(b) = %q", got)
			}
		})
	}
}

// TestSecretToolTokenOnStdin: secret-tool gets the token on stdin, never in
// its arguments, where any process of the machine could read it.
func TestSecretToolTokenOnStdin(t *testing.T) {
	argsLog := fakeSecretTool(t)
	if err := Set("https://h", "dgt_secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := Get("https://h"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argsLog)
	want := "store --label=Dugout https://h service dugout account https://h\nlookup service dugout account https://h\n"
	if string(b) != want {
		t.Fatalf("secret-tool calls = %q, want %q", b, want)
	}
}

// TestFileBackend: without secret-tool the token goes to a 0600 file in a
// 0700 state dir, and Where names that file for login to show.
func TestFileBackend(t *testing.T) {
	dir := fileBackend(t)
	path := filepath.Join(dir, FileName)
	if got := Where(); got != path {
		t.Fatalf("Where = %q, want %q", got, path)
	}
	if err := Set("https://h", "dgt_secret"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v, %v; want mode 0600", st, err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("state dir: %v, %v; want mode 0700", st, err)
	}
	if err := os.WriteFile(path, []byte("{garbage dgt_secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Get("https://h")
	if err == nil || errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), "dgt_secret") {
		t.Fatalf("Get from a corrupt file = %v, want a plain error without the token", err)
	}
	if err := Set("https://h", "dgt_new"); err == nil {
		t.Fatal("Set over a corrupt file dropped the other tokens in it")
	}
}

// TestWhere: a keyring is "the keychain", on both platforms.
func TestWhere(t *testing.T) {
	onPlatform(t, "darwin")
	if got := Where(); got != "the keychain" {
		t.Fatalf("macOS: Where = %q", got)
	}
	fakeSecretTool(t)
	if got := Where(); got != "the keychain" {
		t.Fatalf("secret-tool: Where = %q", got)
	}
}

// failing installs tool as script: security on the PATH on a Mac, the
// injected secret-tool on Linux.
func failing(t *testing.T, tool, script string) {
	t.Helper()
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, tool), script)
	if tool == "security" {
		onPlatform(t, "darwin")
		t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
		return
	}
	withSecretTool(t, filepath.Join(dir, tool))
}

func TestKeychainErrorsNeverCarryTheToken(t *testing.T) {
	for _, tool := range []string{"security", "secret-tool"} {
		t.Run(tool, func(t *testing.T) {
			// secret-tool exits 1 for a failure too, and says why on stderr.
			failing(t, tool, "#!/bin/sh\necho 'locked' >&2\nexit 1\n")
			err := Set("http://x", "dgt_secret")
			if err == nil || strings.Contains(err.Error(), "dgt_secret") || !strings.Contains(err.Error(), "locked") {
				t.Fatalf("Set error = %v", err)
			}
			if _, err := Get("http://x"); err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("Get with a locked keychain = %v, want a plain error", err)
			}
		})
	}
}

func TestKeychainTimeout(t *testing.T) {
	for _, tool := range []string{"security", "secret-tool"} {
		t.Run(tool, func(t *testing.T) {
			failing(t, tool, "#!/bin/sh\nexec sleep 30\n")
			old := timeout
			timeout = 200 * time.Millisecond
			t.Cleanup(func() { timeout = old })
			if _, err := Get("http://x"); err != errTimeout {
				t.Fatalf("Get = %v, want the timeout error", err)
			}
			if err := Set("http://x", "dgt_secret"); err != errTimeout {
				t.Fatalf("Set = %v, want the timeout error", err)
			}
		})
	}
}
