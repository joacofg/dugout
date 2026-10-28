package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"github.com/joacofg/dugout/internal/keychain"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

// tokenFormat is what serve issues: dgt_ and 32 random bytes in base64url.
var tokenFormat = regexp.MustCompile(`^dgt_[A-Za-z0-9_-]{43}$`)

// keychainAccount is the Keychain account of a server: its URL without
// spaces or a trailing slash, so https://h/ and https://h share one item.
func keychainAccount(server string) string {
	return strings.TrimRight(strings.TrimSpace(server), "/")
}

func cmdLogin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := flags.String("server", os.Getenv(envServer), "URL of dugout serve (or $"+envServer+")")
	insecure := flags.Bool("insecure", false, "allow an http:// server outside this machine: the token travels in the clear")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	account := keychainAccount(*server)
	if account == "" {
		fmt.Fprintln(stderr, "dugout login: missing -server")
		return 2
	}
	if wire.InsecureServer(account) && !*insecure {
		fmt.Fprintf(stderr, "dugout login: %s: %v\n", account, wire.ErrInsecureServer)
		return 2
	}
	token, err := readToken(stdin, account, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "dugout login:", err)
		return 1
	}
	if !tokenFormat.MatchString(token) {
		fmt.Fprintln(stderr, "dugout login: that token does not look like a Dugout token (it starts with dgt_)")
		return 1
	}
	if err := keychain.Set(account, token); err != nil {
		fmt.Fprintln(stderr, "dugout login:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Token saved in %s for %s.\n", keychain.Where(), account)
	if wire.InsecureServer(account) {
		fmt.Fprintf(stdout, "Warning: %s has no TLS; the token travels in the clear.\n", account)
		fmt.Fprintf(stdout, "If the daemon is already running, restart it with dugout install -insecure -server %s.\n", account)
		return 0
	}
	fmt.Fprintf(stdout, "If the daemon is already running, restart it with dugout install -server %s.\n", account)
	return 0
}

// readToken reads one line. On a terminal it prompts and turns the echo off
// around the read (stty on /dev/tty: no new dependency), and fails rather
// than read with the echo on; otherwise it just reads, which is what a pipe
// and the tests do. A signal during the hidden read restores the echo first.
func readToken(stdin io.Reader, server string, stderr io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && isTerminal(f) {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return "", errors.New("could not open the terminal to read the token without echo")
		}
		defer tty.Close()
		fmt.Fprintf(stderr, "Token for %s: ", server)
		if err := stty(tty, "-echo"); err != nil {
			return "", fmt.Errorf("could not turn off the terminal echo: %w", err)
		}
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
		restore := func() {
			if err := stty(tty, "echo"); err != nil {
				fmt.Fprintln(stderr, "\ndugout login: could not turn the terminal echo back on (run stty echo):", err)
			}
		}
		go func() {
			if sig, ok := <-sigs; ok {
				restore()
				fmt.Fprintln(stderr)
				signal.Stop(sigs)
				os.Exit(128 + int(sig.(syscall.Signal)))
			}
		}()
		defer func() { signal.Stop(sigs); close(sigs); restore(); fmt.Fprintln(stderr) }()
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("could not read the token: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// /dev/null is a character device too, and it is what a shell without a
	// terminal (an agent's, or curl | sh) gives a command.
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(st, null)
}

func stty(tty *os.File, arg string) error {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = tty
	return cmd.Run()
}

// cmdPause and cmdResume set and clear this machine's pause: a file the
// daemon looks at in every hook, so no daemon needs to be running.
func cmdPause(stdout, stderr io.Writer) int {
	dir, err := statedir.Dir()
	if err == nil {
		err = statedir.Ensure(dir)
	}
	if err == nil {
		err = os.WriteFile(statedir.PausedPath(dir), nil, 0o600)
	}
	if err != nil {
		fmt.Fprintln(stderr, "dugout pause:", err)
		return 1
	}
	fmt.Fprintln(stdout, "Dugout paused on this machine.")
	return 0
}

func cmdResume(stdout, stderr io.Writer) int {
	dir, err := statedir.Dir()
	if err == nil {
		err = os.Remove(statedir.PausedPath(dir))
		if os.IsNotExist(err) {
			err = nil
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "dugout resume:", err)
		return 1
	}
	fmt.Fprintln(stdout, "Dugout resumed on this machine.")
	return 0
}
