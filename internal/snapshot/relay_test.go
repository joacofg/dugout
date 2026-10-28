package snapshot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/testgit"
	"github.com/joacofg/dugout/internal/textual"
)

// remotePair is two machines that share an origin: a clone and a store each.
type remotePair struct {
	f              testgit.Fixture
	clone1, clone2 string
	s1, s2         *Store
}

func newRemotePair(t *testing.T) remotePair {
	t.Helper()
	f, s1 := takeFixture(t)
	clone2 := filepath.Join(f.Base, "clone2")
	testgit.Git(t, f.Base, "clone", "-q", f.Origin, clone2)
	s2 := openStore(t)
	scan := scanOf(t, clone2)
	if err := s2.SetAlternates([]string{filepath.Join(scan.CommonDir, "objects")}); err != nil {
		t.Fatal(err)
	}
	return remotePair{f: f, clone1: f.Clone, clone2: clone2, s1: s1, s2: s2}
}

// changed takes a snapshot of clone 1 with a.go added.
func (p remotePair) changed(t *testing.T) Snap {
	t.Helper()
	testgit.WriteFile(t, filepath.Join(p.clone1, "a.go"), "package a\n")
	snap, _ := take(t, p.s1, p.clone1)
	return snap
}

func refExists(t *testing.T, s *Store, ref string) bool {
	t.Helper()
	return exec.Command("git", "--git-dir="+s.Dir, "rev-parse", "-q", "--verify", ref).Run() == nil
}

func TestBundleImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	p := newRemotePair(t)
	snap := p.changed(t)
	bundle, err := p.s1.Bundle(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.s2.Import(ctx, testWS, bundle, snap); err != nil {
		t.Fatal(err)
	}
	if got := testgit.Git(t, p.s2.Dir, "rev-parse", PeerRef(testWS)); got != snap.Commit {
		t.Fatalf("peer ref = %s, want %s", got, snap.Commit)
	}
	files, err := textual.Changed(ctx, p.s2.Dir, snap.Base, snap.Commit)
	if err != nil || !slices.Equal(files, []string{"a.go"}) {
		t.Fatalf("Changed = %v, %v; want [a.go]", files, err)
	}
	if refExists(t, p.s2, "refs/dugout/incoming") {
		t.Fatal("incoming ref left behind")
	}
}

// TestBundleEmptyChange: a snapshot whose tree is its base's bundles and
// imports like any other, so the other side learns the change is gone.
func TestBundleEmptyChange(t *testing.T) {
	ctx := context.Background()
	p := newRemotePair(t)
	snap, _ := take(t, p.s1, p.clone1)
	bundle, err := p.s1.Bundle(ctx, snap)
	if err != nil {
		t.Fatalf("Bundle of an empty change: %v", err)
	}
	if err := p.s2.Import(ctx, testWS, bundle, snap); err != nil {
		t.Fatalf("Import of an empty change: %v", err)
	}
	if got := testgit.Git(t, p.s2.Dir, "rev-parse", PeerRef(testWS)); got != snap.Commit {
		t.Fatalf("peer ref = %s, want %s", got, snap.Commit)
	}
	if files, err := textual.Changed(ctx, p.s2.Dir, snap.Base, snap.Commit); err != nil || len(files) != 0 {
		t.Fatalf("Changed = %v, %v; want none", files, err)
	}
}

// TestBundleUsesScannedCommit: Bundle packs the snapshot it is given, the one
// the secret scan read, even after a later Take moved the workstream's ref.
func TestBundleUsesScannedCommit(t *testing.T) {
	ctx := context.Background()
	p := newRemotePair(t)
	old := p.changed(t)
	testgit.WriteFile(t, filepath.Join(p.clone1, "b.go"), "package b\n")
	newer, _ := take(t, p.s1, p.clone1)
	if newer.Commit == old.Commit {
		t.Fatal("the second Take wrote the same commit: the test proves nothing")
	}
	bundle, err := p.s1.Bundle(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "b.bundle")
	if err := os.WriteFile(file, bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	heads := strings.Split(testgit.Git(t, p.s1.Dir, "bundle", "list-heads", file), "\n")
	if len(heads) != 1 || !strings.HasPrefix(heads[0], old.Commit+" ") {
		t.Fatalf("bundle heads = %q, want only %s", heads, old.Commit)
	}
	if err := p.s2.Import(ctx, testWS, bundle, old); err != nil {
		t.Fatal(err)
	}
	if refExists(t, p.s1, "refs/dugout/outgoing") {
		t.Fatal("outgoing ref left behind")
	}
}

func TestImportRejectsWrongCommit(t *testing.T) {
	ctx := context.Background()
	p := newRemotePair(t)
	snap1 := p.changed(t)
	b1, err := p.s1.Bundle(ctx, snap1)
	if err != nil {
		t.Fatal(err)
	}
	// A newer snapshot of the same base, as the other daemon uploads while
	// this one is still importing the previous one.
	testgit.WriteFile(t, filepath.Join(p.clone1, "b.go"), "package b\n")
	snap2, _ := take(t, p.s1, p.clone1)
	b2, err := p.s1.Bundle(ctx, snap2)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.s2.Import(ctx, "d9:other", b2, snap2); err != nil {
		t.Fatal(err)
	}
	// Same base and parent as announced, so only the commit check can tell.
	if err := p.s2.Import(ctx, testWS, b1, snap2); err == nil {
		t.Fatal("Import accepted a bundle whose head is not the announced commit")
	}
	if refExists(t, p.s2, PeerRef(testWS)) {
		t.Fatal("peer ref written for a rejected bundle")
	}
}

func TestImportRejectsWrongParent(t *testing.T) {
	ctx := context.Background()
	p := newRemotePair(t)
	snap := p.changed(t)
	bundle, err := p.s1.Bundle(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	want := Snap{Commit: snap.Commit, Base: snap.Commit}
	if err := p.s2.Import(ctx, testWS, bundle, want); err == nil {
		t.Fatal("Import accepted a bundle with another parent")
	}
	if refExists(t, p.s2, PeerRef(testWS)) {
		t.Fatal("peer ref written for a rejected bundle")
	}
	if refExists(t, p.s2, "refs/dugout/incoming") {
		t.Fatal("incoming ref left behind")
	}
}

func TestImportMissingBase(t *testing.T) {
	ctx := context.Background()
	p := newRemotePair(t)
	// Origin moves on; only clone 1 gets the new commit and rebases onto it.
	testgit.WriteFile(t, filepath.Join(p.f.Seed, "new.txt"), "new\n")
	testgit.Git(t, p.f.Seed, "add", "-A")
	testgit.Git(t, p.f.Seed, "commit", "-q", "-m", "next")
	testgit.Git(t, p.f.Seed, "push", "-q", "origin", "main")
	testgit.Git(t, p.clone1, "fetch", "-q", "origin")
	testgit.Git(t, p.clone1, "rebase", "-q", "origin/main")
	snap := p.changed(t)
	bundle, err := p.s1.Bundle(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.s2.Import(ctx, testWS, bundle, snap); !errors.Is(err, ErrMissingBase) {
		t.Fatalf("Import = %v, want ErrMissingBase", err)
	}
	if err := p.s2.FetchBase(ctx, p.f.Origin, "main", ""); err != nil {
		t.Fatal(err)
	}
	if err := p.s2.Import(ctx, testWS, bundle, snap); err != nil {
		t.Fatalf("Import after FetchBase: %v", err)
	}
	if got := testgit.Git(t, p.s2.Dir, "rev-parse", "refs/dugout/base/main"); got != snap.Base {
		t.Fatalf("base ref = %s, want %s", got, snap.Base)
	}
}

// loggingGit puts a git wrapper first in PATH that records, per call, the
// LC_ALL it ran with and its arguments, then runs the real git.
func loggingGit(t *testing.T) (log string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	script := "#!/bin/sh\necho \"LC_ALL=$LC_ALL $*\" >> '" + log + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestImportVerifiesInEnglish(t *testing.T) {
	ctx := context.Background()
	p := newRemotePair(t)
	snap := p.changed(t)
	bundle, err := p.s1.Bundle(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LC_ALL", "es_AR.UTF-8")
	log := loggingGit(t)
	if err := p.s2.Import(ctx, testWS, bundle, snap); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(log)
	var verify []string
	for _, l := range strings.Split(string(body), "\n") {
		if strings.Contains(l, " bundle verify ") {
			verify = append(verify, l)
		}
	}
	if len(verify) != 1 || !strings.HasPrefix(verify[0], "LC_ALL=C ") {
		t.Fatalf("bundle verify calls = %q, want one with LC_ALL=C", verify)
	}
}

func TestFetchBaseNeverAsks(t *testing.T) {
	log := loggingGit(t)
	s := openStore(t)
	_ = s.FetchBase(context.Background(), "https://127.0.0.1:1/x.git", "main", "")
	body, _ := os.ReadFile(log)
	if !strings.Contains(string(body), "-c credential.interactive=false ") {
		t.Fatalf("fetch ran without credential.interactive=false:\n%s", body)
	}
}

func TestFetchEnvDropsRedirectsAndAskpass(t *testing.T) {
	in := []string{
		"GIT_COMMON_DIR=/u/.git", "GIT_NAMESPACE=n", "GIT_ASKPASS=/a", "SSH_ASKPASS=/b",
		"GIT_CONFIG_GLOBAL=/u/gitconfig", "GIT_CONFIG_NOSYSTEM=1", "HOME=/h",
	}
	env := fetchEnv(in, "")
	for _, kv := range env {
		for _, bad := range []string{"GIT_COMMON_DIR=", "GIT_NAMESPACE=", "GIT_ASKPASS=", "SSH_ASKPASS="} {
			if strings.HasPrefix(kv, bad) {
				t.Errorf("env keeps %s", kv)
			}
		}
	}
	for _, want := range []string{"GIT_CONFIG_GLOBAL=/u/gitconfig", "GIT_CONFIG_NOSYSTEM=1", "HOME=/h"} {
		if !slices.Contains(env, want) {
			t.Errorf("env lost %s", want)
		}
	}
}

func TestFetchBaseNoPrompt(t *testing.T) {
	s := openStore(t)
	start := time.Now()
	err := s.FetchBase(context.Background(), "https://127.0.0.1:1/x.git", "main", "")
	if err == nil {
		t.Fatal("FetchBase of an unreachable url succeeded")
	}
	if d := time.Since(start); d > 60*time.Second {
		t.Fatalf("FetchBase took %v", d)
	}
	env := fetchEnv([]string{"GIT_DIR=/x", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=a", "PATH=/bin"}, "")
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "PATH=/bin"} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %s: %v", want, env)
		}
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_CONFIG_") {
			t.Errorf("env keeps %s", kv)
		}
	}
}

// TestFetchEnvSSH: the fetch runs ssh as the user's git in their clone
// would: GIT_SSH_COMMAND from the environment, else the clone's
// core.sshCommand, else a legacy GIT_SSH, else ssh in batch mode.
func TestFetchEnvSSH(t *testing.T) {
	const batch = "GIT_SSH_COMMAND=ssh -o BatchMode=yes"
	for _, c := range []struct {
		name    string
		environ []string
		clone   string
		want    []string // the GIT_SSH_COMMAND and GIT_SSH entries, in order
	}{
		{"environment wins", []string{"GIT_SSH_COMMAND=ssh -i /env"}, "ssh -i /clone", []string{"GIT_SSH_COMMAND=ssh -i /env"}},
		{"clone's core.sshCommand", nil, "ssh -i /clone", []string{"GIT_SSH_COMMAND=ssh -i /clone"}},
		{"clone's over legacy GIT_SSH", []string{"GIT_SSH=/bin/plink"}, "ssh -i /clone", []string{"GIT_SSH=/bin/plink", "GIT_SSH_COMMAND=ssh -i /clone"}},
		{"legacy GIT_SSH", []string{"GIT_SSH=/bin/plink"}, "", []string{"GIT_SSH=/bin/plink"}},
		{"none: batch mode", []string{"HOME=/h"}, "", []string{batch}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, kv := range fetchEnv(c.environ, c.clone) {
				if strings.HasPrefix(kv, "GIT_SSH_COMMAND=") || strings.HasPrefix(kv, "GIT_SSH=") {
					got = append(got, kv)
				}
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("ssh entries = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSecretFound(t *testing.T) {
	ctx := context.Background()
	p := newRemotePair(t)
	key := "AKIA" + strings.Repeat("Q", 16)
	testgit.WriteFile(t, filepath.Join(p.clone1, "ok.go"), "package ok\n")
	testgit.WriteFile(t, filepath.Join(p.clone1, "cfg", "aws.go"), "var k = \""+key+"\"\n")
	snap, _ := take(t, p.s1, p.clone1)
	path, rule, err := p.s1.Secret(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if path != "cfg/aws.go" || rule != "aws-access-key" {
		t.Fatalf("Secret = %q, %q", path, rule)
	}
	if strings.Contains(path+rule, key) {
		t.Fatal("result carries the value")
	}
}

func TestSecretFoundInTypeChange(t *testing.T) {
	ctx := context.Background()
	p := newRemotePair(t)
	// A symlink at the base that becomes a regular file holding a key.
	if err := os.Symlink("a.txt", filepath.Join(p.f.Seed, "link")); err != nil {
		t.Fatal(err)
	}
	testgit.Git(t, p.f.Seed, "add", "-A")
	testgit.Git(t, p.f.Seed, "commit", "-q", "-m", "link")
	testgit.Git(t, p.f.Seed, "push", "-q", "origin", "main")
	testgit.Git(t, p.clone1, "fetch", "-q", "origin")
	testgit.Git(t, p.clone1, "rebase", "-q", "origin/main")
	if err := os.Remove(filepath.Join(p.clone1, "link")); err != nil {
		t.Fatal(err)
	}
	testgit.WriteFile(t, filepath.Join(p.clone1, "link"), "k = \"AKIA"+strings.Repeat("Q", 16)+"\"\n")
	snap, _ := take(t, p.s1, p.clone1)
	path, rule, err := p.s1.Secret(ctx, snap)
	if err != nil || path != "link" || rule != "aws-access-key" {
		t.Fatalf("Secret = %q, %q, %v", path, rule, err)
	}
}

// TestNarrow: a narrowed snapshot changes the paths asked for, a deletion
// included, as the snapshot has them, and nothing else of it, so a secret in
// another file stays home; it is on the same base, the same inputs give the
// same commit, and a path the snapshot did not change changes nothing.
func TestNarrow(t *testing.T) {
	ctx := context.Background()
	p := newRemotePair(t)
	testgit.WriteFile(t, filepath.Join(p.clone1, "shared.go"), "mine\n")
	testgit.WriteFile(t, filepath.Join(p.clone1, "c.txt"), "other\n")
	testgit.WriteFile(t, filepath.Join(p.clone1, "notes.txt"), "k = \"AKIA"+strings.Repeat("Q", 16)+"\"\n")
	if err := os.Remove(filepath.Join(p.clone1, "a.txt")); err != nil {
		t.Fatal(err)
	}
	full, _ := take(t, p.s1, p.clone1)

	snap, err := p.s1.Narrow(ctx, full, []string{"shared.go", "a.txt", "never.go"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Base != full.Base || snap.Commit == full.Commit {
		t.Fatalf("Narrow = %+v of %+v, want another commit on the same base", snap, full)
	}
	out, err := exec.Command("git", "--git-dir="+p.s1.Dir, "diff-tree", "-r", "--name-status", snap.Base, snap.Commit).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(out)); !slices.Equal(got, []string{"D", "a.txt", "M", "shared.go"}) {
		t.Fatalf("narrowed changes = %v, want a.txt deleted and shared.go modified", got)
	}
	if path, _, err := p.s1.Secret(ctx, snap); err != nil || path != "" {
		t.Fatalf("Secret of the narrowed snapshot = %q, %v; want none: notes.txt stays home", path, err)
	}
	again, err := p.s1.Narrow(ctx, full, []string{"a.txt", "shared.go"})
	if err != nil || again != snap {
		t.Fatalf("Narrow again = %+v, %v; want %+v", again, err, snap)
	}
	none, err := p.s1.Narrow(ctx, full, []string{"never.go"})
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("git", "--git-dir="+p.s1.Dir, "diff-tree", "-r", "--name-only", none.Base, none.Commit).Output(); len(out) != 0 {
		t.Fatalf("Narrow to an unchanged path changes %q, want nothing", out)
	}
}

func TestSecretNone(t *testing.T) {
	p := newRemotePair(t)
	snap := p.changed(t)
	path, rule, err := p.s1.Secret(context.Background(), snap)
	if err != nil || path != "" || rule != "" {
		t.Fatalf("Secret = %q, %q, %v; want none", path, rule, err)
	}
}

func TestPrunePeers(t *testing.T) {
	ctx := context.Background()
	p := newRemotePair(t)
	snap := p.changed(t)
	bundle, err := p.s1.Bundle(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{"d2:one", "d3:two"} {
		if err := p.s2.Import(ctx, ws, bundle, snap); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.s2.PrunePeers(ctx, []string{"d2:one"}); err != nil {
		t.Fatal(err)
	}
	if !refExists(t, p.s2, PeerRef("d2:one")) || refExists(t, p.s2, PeerRef("d3:two")) {
		t.Fatal("PrunePeers did not keep exactly d2:one")
	}
}
