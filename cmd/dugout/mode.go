package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joacofg/dugout/internal/daemon"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

// interactive reports whether init may ask the person for a mode; a var so a
// test can answer through a pipe.
var interactive = func(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	return ok && isTerminal(f)
}

// askMode asks the daemon for the mode of the repo that holds path, absolute,
// setting it first when set is not "".
func askMode(sock, path, set string) (wire.Mode, error) {
	var m wire.Mode
	resp, err := ask(sock, wire.Request{Type: wire.TypeMode, Path: path, Text: set})
	if err != nil {
		return m, fmt.Errorf("the daemon is not responding: %w", err)
	}
	if resp.Error != "" {
		return m, errors.New(resp.Error)
	}
	return m, json.Unmarshal(resp.Output, &m)
}

// describeMode is the line that tells the person a repo's mode.
func describeMode(m wire.Mode) string {
	mode := daemon.Mode(m.Mode)
	name := string(mode.Or())
	if mode == "" {
		name += " (default)"
	}
	return fmt.Sprintf("Dugout mode in %s: %s: %s", m.Repo, name, mode.Summary())
}

const switchHint = "Change it with dugout mode auto|scoped|supervised, or with /dugout in Claude Code."

// cmdMode shows, or with an argument sets, the autonomy mode of the repo of
// the current directory: for this person, on this machine.
func cmdMode(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	set := ""
	if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
		m, err := daemon.ParseMode(args[0])
		if err != nil {
			fmt.Fprintln(stderr, "dugout mode:", err)
			return 2
		}
		set = string(m)
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "dugout mode:", err)
		return 1
	}
	sock, err := statedir.SocketPath()
	if err != nil {
		fmt.Fprintln(stderr, "dugout mode:", err)
		return 1
	}
	m, err := askMode(sock, cwd, set)
	if err != nil {
		fmt.Fprintln(stderr, "dugout mode:", err)
		return 1
	}
	fmt.Fprintln(stdout, describeMode(m))
	if set == "" {
		fmt.Fprintln(stdout, switchHint)
	}
	return 0
}

// cmdInit sets Dugout up for a repo: it tracks the worktree and asks, once,
// the autonomy mode its agents get. -mode answers without asking; a repo whose
// mode was chosen is not asked again; without a terminal and -mode, it stays
// in the default.
func cmdInit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	modeFlag := flags.String("mode", "", "autonomy mode: auto, scoped or supervised (without it, it asks)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 1 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	set := ""
	if *modeFlag != "" {
		m, err := daemon.ParseMode(*modeFlag)
		if err != nil {
			fmt.Fprintln(stderr, "dugout init:", err)
			return 2
		}
		set = string(m)
	}
	path, err := filepath.Abs(flags.Arg(0)) // "" is the current directory
	if err != nil {
		fmt.Fprintln(stderr, "dugout init:", err)
		return 1
	}
	sock, err := statedir.SocketPath()
	if err != nil {
		fmt.Fprintln(stderr, "dugout init:", err)
		return 1
	}
	resp, err := ask(sock, wire.Request{Type: wire.TypeTrack, Path: path})
	if err != nil {
		fmt.Fprintln(stderr, "dugout init: the daemon is not responding:", err)
		return 1
	}
	if resp.Error != "" {
		fmt.Fprintln(stderr, "dugout init:", resp.Error)
		return 1
	}
	m, err := askMode(sock, path, "")
	if err != nil {
		fmt.Fprintln(stderr, "dugout init:", err)
		return 1
	}
	if set == "" && m.Mode == "" && interactive(stdin) {
		if set, err = chooseMode(stdin, stdout, m.Repo); err != nil {
			fmt.Fprintln(stderr, "dugout init:", err)
			return 1
		}
	}
	if set != "" {
		if m, err = askMode(sock, path, set); err != nil {
			fmt.Fprintln(stderr, "dugout init:", err)
			return 1
		}
	}
	fmt.Fprintln(stdout, describeMode(m))
	fmt.Fprintln(stdout, switchHint)
	return 0
}

// chooseMode asks the person which mode the agents of repo get, auto when
// they just press enter, until they name one.
func chooseMode(stdin io.Reader, stdout io.Writer, repo string) (string, error) {
	fmt.Fprintf(stdout, "How much autonomy do your agents have in %s when they conflict with others'?\n", repo)
	for i, m := range daemon.Modes {
		rec := ""
		if m == daemon.ModeAuto {
			rec = " (recommended)"
		}
		fmt.Fprintf(stdout, "  %d) %s%s: %s\n", i+1, m, rec, m.Summary())
	}
	in := bufio.NewReader(stdin)
	for {
		fmt.Fprint(stdout, "Choose [1]: ")
		line, err := in.ReadString('\n')
		answer := strings.TrimSpace(line)
		if answer == "" && err == nil {
			return string(daemon.ModeAuto), nil
		}
		if n, convErr := strconv.Atoi(answer); convErr == nil && n >= 1 && n <= len(daemon.Modes) {
			return string(daemon.Modes[n-1]), nil
		}
		if m, parseErr := daemon.ParseMode(answer); parseErr == nil {
			return string(m), nil
		}
		if err != nil {
			return "", errors.New("you did not choose a mode")
		}
		fmt.Fprintln(stdout, "Choose 1, 2 or 3, or type auto, scoped or supervised.")
	}
}
