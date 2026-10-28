package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

const officeTimeout = 10 * time.Second

// openURL opens u in the browser; a var so tests can catch it.
var openURL = defaultOpenURL

func defaultOpenURL(u string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, u).Run()
}

// cmdOffice is `dugout office`: it asks serve for a read-only view token as
// this person and opens the live office view with it. The server is -server,
// else $DUGOUT_SERVER, else the running daemon's.
func cmdOffice(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("office", flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := flags.String("server", os.Getenv(envServer), "URL of dugout serve (or $"+envServer+", else the daemon's)")
	noOpen := flags.Bool("no-open", false, "print the URL without opening a browser")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	if *server == "" {
		*server = daemonServer()
	}
	base := keychainAccount(*server)
	if base == "" || base == "off" {
		fmt.Fprintln(stderr, "dugout office: missing -server (no $"+envServer+" and no daemon with a server)")
		return 2
	}
	token, err := resolveToken(base)
	if err != nil {
		fmt.Fprintln(stderr, "dugout office:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), officeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/office/token", bytes.NewReader([]byte("{}")))
	if err != nil {
		fmt.Fprintln(stderr, "dugout office:", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "dugout office: the server did not answer:", err)
		return 1
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		fmt.Fprintln(stderr, "dugout office: this server is older than the office view: update dugout serve")
		return 1
	case http.StatusUnauthorized:
		fmt.Fprintf(stderr, "dugout office: the server refused your token: run dugout login -server %s\n", base)
		return 1
	default:
		fmt.Fprintf(stderr, "dugout office: the server answered %s\n", resp.Status)
		return 1
	}
	var vt wire.ViewToken
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&vt); err != nil || !strings.HasPrefix(vt.Token, wire.ViewTokenPrefix) {
		fmt.Fprintln(stderr, "dugout office: unexpected answer from the server")
		return 1
	}
	u := base + "/office#t=" + vt.Token
	fmt.Fprintf(stdout, "%s\nRead-only, valid until %s.\n", u, vt.Expires.Local().Format("15:04 on Jan 2"))
	if !*noOpen {
		if err := openURL(u); err != nil {
			fmt.Fprintln(stderr, "dugout office: could not open a browser; open the URL above:", err)
		}
	}
	return 0
}

// daemonServer is the running daemon's server, "" when there is no daemon or
// it runs without one.
func daemonServer() string {
	sock, err := statedir.SocketPath()
	if err != nil {
		return ""
	}
	resp, err := ask(sock, wire.Request{Type: wire.TypeStatus})
	if err != nil || resp.Error != "" {
		return ""
	}
	var st wire.Status
	if json.Unmarshal(resp.Output, &st) != nil {
		return ""
	}
	return st.Server
}
