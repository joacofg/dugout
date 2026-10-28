package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/joacofg/dugout/internal/keychain"
	"github.com/joacofg/dugout/internal/wire"
)

// installerURL is the one-line installer, for the invite to say how to get
// dugout first.
const installerURL = "https://raw.githubusercontent.com/joacofg/dugout/main/install.sh"

// inviteFormat is what serve issues: dgi_ and 32 random bytes in base64url.
var inviteFormat = regexp.MustCompile(`^dgi_[A-Za-z0-9_-]{43}$`)

// inviteLink is what an invite carries: the server's URL, #, the code.
func inviteLink(server, code string) string { return server + "#" + code }

// parseInvite splits an invite into the server's URL and the code.
func parseInvite(raw string) (server, code string, err error) {
	raw = strings.TrimSpace(raw)
	i := strings.LastIndex(raw, "#")
	if i < 0 {
		return "", "", errors.New("that is not a Dugout invite: it looks like https://server#dgi_…")
	}
	server, code = keychainAccount(raw[:i]), raw[i+1:]
	u, perr := url.Parse(server)
	if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.User != nil {
		return "", "", errors.New("that is not a Dugout invite: it needs the server's http(s) URL before the #")
	}
	if !inviteFormat.MatchString(code) {
		return "", "", errors.New("that is not a Dugout invite: the code after the # starts with dgi_")
	}
	return server, code, nil
}

// cmdJoin redeems an invite for this person's token, keeps the token as
// login does, then runs setup against the invite's server. Neither the token
// nor the code is ever printed: an agent may be the one running it.
func cmdJoin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("join", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var o setupOpts
	o.register(flags, false)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	server, code, err := parseInvite(flags.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "dugout join:", err)
		return 2
	}
	if wire.InsecureServer(server) {
		if !o.insecure {
			fmt.Fprintf(stderr, "dugout join: %s: %v\n", server, wire.ErrInsecureServer)
			return 2
		}
		fmt.Fprintf(stderr, "warning: %s has no TLS; your token travels in the clear\n", server)
	}
	name, token, err := redeem(server, code)
	if err == nil && !tokenFormat.MatchString(token) {
		err = errors.New("serve answered something that is not a Dugout token")
	}
	if err != nil {
		fmt.Fprintln(stderr, "dugout join:", err)
		return 1
	}
	if err := keychain.Set(server, token); err != nil {
		fmt.Fprintln(stderr, "dugout join:", err)
		fmt.Fprintf(stderr, "The invite is now spent and the token was not stored: ask your admin to run `dugout admin token revoke %s` and invite you again.\n", name)
		return 1
	}
	fmt.Fprintf(stdout, "Joined %s as %s. Your token is in %s.\n", server, name, keychain.Where())
	o.server = server
	return runSetup(o, stdin, stdout, stderr)
}

// redeem spends the invite code at server for its person's name and token.
func redeem(server, code string) (name, token string, err error) {
	body, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequest(http.MethodPost, server+"/v1/join", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: adminTimeout}).Do(req)
	if err != nil {
		if ue, ok := err.(*url.Error); ok {
			err = ue.Err
		}
		return "", "", fmt.Errorf("serve is not responding at %s: %v", server, err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusCreated:
	case resp.StatusCode == http.StatusUnauthorized:
		return "", "", errors.New("this invite does not work: it was already used, it expired or it is not from this server. Ask for a new one")
	case resp.StatusCode == http.StatusNotFound:
		return "", "", fmt.Errorf("%s takes no invites: it needs a newer dugout serve, run with -state", server)
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && len(bytes.TrimSpace(msg)) > 0:
		return "", "", errors.New(strings.TrimSpace(string(msg)))
	default:
		return "", "", fmt.Errorf("serve answered %d", resp.StatusCode)
	}
	var out struct{ Name, Token string }
	if err := json.Unmarshal(msg, &out); err != nil {
		return "", "", errors.New("unreadable response from serve")
	}
	return out.Name, out.Token, nil
}
