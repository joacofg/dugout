package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/testgit"
	"github.com/joacofg/dugout/internal/wire"
)

// TestNoSecretReachesAgent: with a daemon that holds the person's token and
// an invite pending on serve, nothing an agent can read carries a token or
// an invite: no hook's output, no MCP result (office_who included), no
// dugout status or doctor line, and neither serve's log nor the daemon's.
// An agent's context ends up in its provider's logs.
func TestNoSecretReachesAgent(t *testing.T) {
	adm := startAdminServe(t)
	out, errOut, code := adm.admin(t, adm.adminTok, "token", "issue", "ana")
	if code != 0 {
		t.Fatalf("token issue: %d %s", code, errOut)
	}
	anaTok := strings.TrimSpace(out[strings.LastIndex(out, " ")+1:])
	out, errOut, code = adm.admin(t, adm.adminTok, "invite", "dani")
	m := inviteLine.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("admin invite: %d %s %s", code, out, errOut)
	}
	invite := m[1][strings.LastIndex(m[1], "#")+1:]

	f := testgit.NewFixture(t)
	testgit.Git(t, f.Clone, "switch", "-q", "-c", "feat-a")
	testgit.WriteFile(t, filepath.Join(f.Clone, "shared.go"), "changed\n")
	home := shortDir(t)
	daemon := start(t, []string{statedir.EnvHome + "=" + home, wire.EnvToken + "=" + anaTok}, "daemon", "-server", "http://"+adm.addr, "-poll", "300ms", "-resend", "1s")
	waitDaemonSocket(t, home)

	var seen []string
	session := map[string]any{"session_id": "sS", "cwd": f.Clone}
	tool := map[string]any{"session_id": "sS", "cwd": f.Clone, "tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(f.Clone, "shared.go")},
		"prompt": "go on"}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"} {
		in := session
		if ev != "SessionStart" {
			in = tool
		}
		seen = append(seen, ev+": "+runHook(t, home, ev, in))
	}
	eventually(t, 10*time.Second, "the daemon reaches serve with ana's token", func() bool {
		return strings.Contains(adm.srv.log.String(), "daemon bound")
	})

	c := startMCP(t, home, f.Clone)
	c.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}})
	var list struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	raw := c.rpc("tools/list", map[string]any{})
	seen = append(seen, "tools/list: "+string(raw))
	if json.Unmarshal(raw, &list) != nil || len(list.Tools) < 6 {
		t.Fatalf("tools/list = %s", raw)
	}
	args := map[string]any{"session_id": "sS", "path": "shared.go", "summary": "fixing the login", "conflict_id": "c1", "reason": "r", "urgency": "low"}
	for _, tl := range list.Tools {
		text, _ := c.tool(tl.Name, args)
		seen = append(seen, tl.Name+": "+text)
	}
	seen = append(seen, "SessionEnd: "+runHook(t, home, "SessionEnd", session))

	for _, sub := range [][]string{{"status"}, {"doctor"}} {
		cmd := exec.Command(bin, sub...)
		cmd.Dir = f.Clone
		cmd.Env = append(os.Environ(), statedir.EnvHome+"="+home, "HOME="+home, envServer+"=http://"+adm.addr, wire.EnvToken+"="+anaTok)
		out, _ := cmd.CombinedOutput()
		seen = append(seen, sub[0]+": "+string(out))
	}
	seen = append(seen, "daemon log: "+daemon.log.String(), "serve log: "+adm.srv.log.String())

	if !strings.Contains(seen[0], "office_declare_intent") || !strings.Contains(strings.Join(seen, "\n"), "office_who: ") {
		t.Fatalf("the agent heard nothing to check:\n%s", strings.Join(seen, "\n"))
	}
	for _, s := range seen {
		for _, secret := range []string{anaTok, invite, "dgt_", "dgi_"} {
			if strings.Contains(s, secret) {
				t.Errorf("%q reaches an agent or a log:\n%s", secret, s)
			}
		}
	}
}
