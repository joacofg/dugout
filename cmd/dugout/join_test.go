package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// inviteLine is the line `dugout admin invite` prints for the person.
var inviteLine = regexp.MustCompile(`(?m)^Join our Dugout: dugout join '(http://[^#']+#dgi_[A-Za-z0-9_-]{43})'$`)

// TestInviteAndJoin: the admin's invite line, run on the teammate's machine,
// redeems for their token, keeps it in the keychain as login does and runs
// setup, whose doctor reaches serve with it; the same line fails a second
// time. Neither the token nor the code is in anything join prints.
func TestInviteAndJoin(t *testing.T) {
	adm := startAdminServe(t)
	out, errOut, code := adm.admin(t, adm.adminTok, "invite", "-expires", "2h", "Dani")
	m := inviteLine.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("admin invite: exit %d\n%s\n%s", code, out, errOut)
	}
	link := m[1]
	if !strings.Contains(out, "For dani only, single use, until ") ||
		!strings.Contains(out, "curl -fsSL "+installerURL+" | sh -s -- join '"+link+"'") ||
		!strings.Contains(out, "is this machine only") {
		t.Fatalf("admin invite output:\n%s", out)
	}

	w := newSetupWorld(t)
	out, errOut, code = w.run(t, "join", link)
	if code != 0 {
		t.Fatalf("join: exit %d\n%s\n%s", code, out, errOut)
	}
	wantLines(t, out, "Joined http://"+adm.addr+" as dani. Your token is in the keychain.",
		"Dugout installed: the daemon starts with your session and talks to http://"+adm.addr+".",
		"✓ the server http://"+adm.addr+" responds and accepts your token")
	store, err := os.ReadFile(w.store)
	if err != nil {
		t.Fatal(err)
	}
	tok := regexp.MustCompile(`dgt_[A-Za-z0-9_-]{43}`).FindString(string(store))
	if tok == "" || !strings.HasPrefix(string(store), "http://"+adm.addr+"\t") {
		t.Fatalf("keychain store: %q", store)
	}
	code6 := link[strings.LastIndex(link, "#")+1:]
	for _, s := range []string{out, errOut} {
		if strings.Contains(s, tok) || strings.Contains(s, code6) || strings.Contains(s, "dgt_") {
			t.Fatalf("join printed a secret:\n%s", s)
		}
	}
	if list, _, _ := adm.admin(t, adm.adminTok, "token", "list"); list != "dani\n" {
		t.Fatalf("token list after join: %q", list)
	}

	out, errOut, code = w.run(t, "join", link)
	if code != 1 || !strings.Contains(errOut, "this invite does not work: it was already used, it expired or it is not from this server") {
		t.Fatalf("second join: exit %d\n%s\n%s", code, out, errOut)
	}
}

// TestJoinRefusesBadInvites: what is not an invite never reaches a server.
func TestJoinRefusesBadInvites(t *testing.T) {
	for _, bad := range []string{"", "https://h", "ftp://h#dgi_" + strings.Repeat("a", 43), "https://h#dgt_" + strings.Repeat("a", 43),
		"#dgi_" + strings.Repeat("a", 43), "https://u:p@h#dgi_" + strings.Repeat("a", 43)} {
		if _, _, err := parseInvite(bad); err == nil {
			t.Errorf("parseInvite(%q) took it", bad)
		}
	}
	server, code, err := parseInvite("  https://dugout.example/#dgi_" + strings.Repeat("a", 43) + "\n")
	if err != nil || server != "https://dugout.example" || code != "dgi_"+strings.Repeat("a", 43) {
		t.Fatalf("parseInvite = %q %q %v", server, code, err)
	}
}

// TestAdminInviteArgs: the flag goes on either side of the name.
func TestAdminInviteArgs(t *testing.T) {
	for _, args := range [][]string{{"invite", "dani"}, {"invite", "-expires", "1h", "dani"}, {"invite", "dani", "-expires", "1h"}} {
		if parseAdminCmd(args) == nil {
			t.Errorf("%q not taken", args)
		}
	}
	for _, args := range [][]string{{"invite"}, {"invite", "dani", "eve"}, {"invite", "-expires", "soon", "dani"}} {
		if parseAdminCmd(args) != nil {
			t.Errorf("%q taken", args)
		}
	}
}
