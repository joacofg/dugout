package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/serve"
	"github.com/joacofg/dugout/internal/wire"
)

const (
	envAdminToken = "DUGOUT_ADMIN_TOKEN"
	adminTimeout  = 15 * time.Second
)

// adminClient talks to serve's /v1/admin API with the admin token from the
// environment (never a flag, so it stays out of ps and shell history).
type adminClient struct {
	base, token string
	http        *http.Client
}

// adminErr is an error already worded for the user.
type adminErr string

func (e adminErr) Error() string { return string(e) }

func newAdminClient() (*adminClient, error) {
	token := os.Getenv(envAdminToken)
	if token == "" {
		return nil, adminErr("missing " + envAdminToken + " (the token dugout admin init printed)")
	}
	base := os.Getenv(envServer)
	if base == "" {
		base = "http://" + defaultAddr
	}
	return &adminClient{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: adminTimeout}}, nil
}

// do sends one request. A non-2xx answer becomes an error: 401 and the
// caller's notFound text are fixed, any other 4xx shows serve's message.
func (c *adminClient) do(method, path string, in, out any, notFound string) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error repeats the request URL; the cause is what matters.
		if ue, ok := err.(*url.Error); ok {
			err = ue.Err
		}
		return adminErr(fmt.Sprintf("serve is not responding at %s: %v", c.base, err))
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return adminErr("serve rejected the admin token")
	case resp.StatusCode == http.StatusNotFound && notFound != "":
		return adminErr(notFound)
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && len(bytes.TrimSpace(msg)) > 0:
		return adminErr(strings.TrimSpace(string(msg)))
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		// Serve's own words, when it says any, capped.
		text := bytes.TrimSpace(msg)
		if len(text) > 512 {
			text = text[:512]
		}
		if len(text) > 0 {
			return adminErr(fmt.Sprintf("serve answered %d: %s", resp.StatusCode, text))
		}
		return adminErr(fmt.Sprintf("serve answered %d", resp.StatusCode))
	}
	if raw, ok := out.(*string); ok {
		*raw = string(msg)
	} else if out != nil {
		if err := json.Unmarshal(msg, out); err != nil {
			return adminErr("unreadable response from serve: " + err.Error())
		}
	}
	return nil
}

// runAdminCmd handles every `dugout admin` command but init.
func runAdminCmd(args []string, stdout, stderr io.Writer) int {
	cmd := parseAdminCmd(args)
	if cmd == nil {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	c, err := newAdminClient()
	if err == nil {
		if wire.InsecureServer(c.base) {
			fmt.Fprintf(stderr, "warning: %s has no TLS; the token travels in the clear\n", c.base)
		}
		var out string
		if out, err = cmd(c); err == nil {
			fmt.Fprint(stdout, out)
			return 0
		}
	}
	fmt.Fprintln(stderr, err)
	return 1
}

// parseAdminCmd maps the arguments to the command, or nil if they are not one.
func parseAdminCmd(args []string) func(*adminClient) (string, error) {
	if args[0] == "invite" {
		return parseInviteCmd(args[1:])
	}
	key := args
	if len(key) > 2 {
		key = key[:2]
	}
	switch strings.Join(key, " ") {
	case "pause":
		return func(c *adminClient) (string, error) { return setPause(c, true) }
	case "resume":
		return func(c *adminClient) (string, error) { return setPause(c, false) }
	case "metrics":
		if len(args) == 1 {
			return metrics
		}
	case "token list":
		if len(args) == 2 {
			return tokenList
		}
	case "repo list":
		if len(args) == 2 {
			return repoList
		}
	case "repo add":
		if len(args) >= 3 {
			return func(c *adminClient) (string, error) { return repoAdd(c, args[2], args[3:]) }
		}
	case "repo grant", "repo ungrant":
		if len(args) == 4 {
			return func(c *adminClient) (string, error) { return repoMembers(c, key[1] == "grant", args[2], args[3]) }
		}
	case "token issue", "token revoke", "repo remove":
		if len(args) != 3 {
			return nil
		}
		arg := args[2]
		switch key[1] {
		case "issue":
			return func(c *adminClient) (string, error) { return tokenIssue(c, arg) }
		case "revoke":
			return func(c *adminClient) (string, error) { return tokenRevoke(c, arg) }
		default:
			return func(c *adminClient) (string, error) { return repoRemove(c, arg) }
		}
	}
	return nil
}

func tokenIssue(c *adminClient, name string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	if err := c.do(http.MethodPost, "/v1/admin/tokens", map[string]string{"name": name}, &out, ""); err != nil {
		return "", err
	}
	return fmt.Sprintf("token for %s (shown only once): %s\n", name, out.Token), nil
}

// parseInviteCmd reads `invite [-expires 24h] <person>`, the flag on either
// side of the name.
func parseInviteCmd(args []string) func(*adminClient) (string, error) {
	flags := flag.NewFlagSet("admin invite", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	expires := flags.Duration("expires", serve.DefaultInviteTTL, "how long the invite works")
	if flags.Parse(args) != nil || flags.NArg() < 1 {
		return nil
	}
	name := flags.Arg(0)
	if flags.Parse(flags.Args()[1:]) != nil || flags.NArg() > 0 {
		return nil
	}
	return func(c *adminClient) (string, error) { return inviteCreate(c, name, *expires) }
}

// inviteCreate prints the line to send the person: their agent or terminal
// runs it, and it works once, before it expires.
func inviteCreate(c *adminClient, name string, expires time.Duration) (string, error) {
	var out struct {
		Name    string    `json:"name"`
		Code    string    `json:"code"`
		Expires time.Time `json:"expires"`
	}
	if err := c.do(http.MethodPost, "/v1/admin/invites", map[string]any{"name": name, "ttl_seconds": int64(expires / time.Second)}, &out, "serve takes no invites: update it"); err != nil {
		return "", err
	}
	link := inviteLink(c.base, out.Code)
	var b strings.Builder
	fmt.Fprintf(&b, "Join our Dugout: dugout join '%s'\n", link)
	fmt.Fprintf(&b, "\nFor %s only, single use, until %s. Send it over a private channel.\n", out.Name, out.Expires.UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&b, "Without dugout yet, this one line installs it and joins: curl -fsSL %s | sh -s -- join '%s'\n", installerURL, link)
	if wire.IsLoopbackURL(c.base) {
		fmt.Fprintf(&b, "Note: %s is this machine only. For a teammate, set %s to the URL they reach serve at.\n", c.base, envServer)
	}
	return b.String(), nil
}

func tokenRevoke(c *adminClient, name string) (string, error) {
	if err := c.do(http.MethodDelete, "/v1/admin/tokens/"+url.PathEscape(name), nil, nil, "there is no token for "+name); err != nil {
		return "", err
	}
	return fmt.Sprintf("token for %s revoked\n", name), nil
}

func tokenList(c *adminClient) (string, error) {
	var users []struct {
		Name    string `json:"name"`
		Revoked bool   `json:"revoked"`
	}
	if err := c.do(http.MethodGet, "/v1/admin/tokens", nil, &users, ""); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, u := range users {
		b.WriteString(u.Name)
		if u.Revoked {
			b.WriteString(" (revoked)")
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// repoEntry is an allowed repo and its members; none means everyone.
type repoEntry struct {
	Repo    string   `json:"repo"`
	Members []string `json:"members"`
}

// only is the members as list and add print them after the identity, ""
// when the repo is everyone's.
func (e repoEntry) only() string {
	if len(e.Members) == 0 {
		return ""
	}
	return ", only for: " + strings.Join(e.Members, ", ")
}

// repos reads the allowlist. A serve from before members answers bare
// identities, which are everyone's.
func repos(c *adminClient) ([]repoEntry, error) {
	var raw []json.RawMessage
	if err := c.do(http.MethodGet, "/v1/admin/repos", nil, &raw, ""); err != nil {
		return nil, err
	}
	out := make([]repoEntry, 0, len(raw))
	for _, r := range raw {
		var e repoEntry
		if json.Unmarshal(r, &e.Repo) != nil && json.Unmarshal(r, &e) != nil {
			return nil, adminErr("unreadable response from serve: " + string(r))
		}
		out = append(out, e)
	}
	return out, nil
}

func repoList(c *adminClient) (string, error) {
	list, err := repos(c)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, e := range list {
		b.WriteString(e.Repo + e.only() + "\n")
	}
	return b.String(), nil
}

// repoAdd prints the identity serve stores, which it derives with the same
// gitscan.NormalizeRemote, and the members it has after the add: those
// given join any it had.
func repoAdd(c *adminClient, repo string, members []string) (string, error) {
	norm := gitscan.NormalizeRemote(repo)
	if norm == "" {
		return "", adminErr("invalid repo")
	}
	body := map[string]any{"repo": repo}
	if len(members) > 0 {
		body["members"] = members
	}
	if err := c.do(http.MethodPost, "/v1/admin/repos", body, nil, ""); err != nil {
		return "", err
	}
	e := repoEntry{Repo: norm}
	list, err := repos(c)
	for _, x := range list {
		if x.Repo == norm {
			e = x
		}
	}
	// A serve from before members allows the repo and drops them: say so,
	// since then everyone sees it.
	if len(members) > 0 && (err != nil || len(e.Members) == 0) {
		return "", adminErr(fmt.Sprintf("serve allowed %s but did not store its members, so everyone sees it: update serve, or remove the repo", norm))
	}
	return fmt.Sprintf("repo %s allowed%s\n", norm, e.only()), nil
}

// repoMembers grants or ungrants a person a repo and prints who sees it now.
func repoMembers(c *adminClient, grant bool, repo, name string) (string, error) {
	norm := gitscan.NormalizeRemote(repo)
	if norm == "" {
		return "", adminErr("invalid repo")
	}
	verb, done := "grant", "now sees"
	if !grant {
		verb, done = "ungrant", "no longer sees"
	}
	var e repoEntry
	if err := c.do(http.MethodPost, "/v1/admin/repos/"+verb, map[string]string{"repo": norm, "name": name}, &e, ""); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s %s (members: %s)\n", strings.ToLower(strings.TrimSpace(name)), done, e.Repo, strings.Join(e.Members, ", ")), nil
}

// repoRemove sends the identity as a query parameter: a path would break on
// # ? % and on a leading /.
func repoRemove(c *adminClient, repo string) (string, error) {
	norm := gitscan.NormalizeRemote(repo)
	if norm == "" {
		return "", adminErr("invalid repo")
	}
	if err := c.do(http.MethodDelete, "/v1/admin/repos?"+url.Values{"repo": {norm}}.Encode(), nil, nil, ""); err != nil {
		return "", err
	}
	return fmt.Sprintf("repo %s removed\n", norm), nil
}

func setPause(c *adminClient, paused bool) (string, error) {
	if err := c.do(http.MethodPost, "/v1/admin/pause", map[string]bool{"paused": paused}, nil, ""); err != nil {
		return "", err
	}
	if paused {
		return "Dugout paused for the whole team\n", nil
	}
	return "Dugout resumed for the whole team\n", nil
}

// metrics is serve's Prometheus text, printed as it comes.
func metrics(c *adminClient) (string, error) {
	var body string
	if err := c.do(http.MethodGet, "/v1/admin/metrics", nil, &body, "serve has no metrics: update it, or run it with -state"); err != nil {
		return "", err
	}
	return body, nil
}
