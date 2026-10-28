package serve

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/statedir"
)

const (
	stateVersion = 1
	tokenPrefix  = "dgt_"
)

// Stored is what serve persists: only tokens and invite codes (hashed), the
// repo allowlist with each repo's members, the pause flag and which person
// each daemon_id is bound to. Workstreams and warnings are rebuilt from the daemons.
type Stored struct {
	Version   int      `json:"version"`
	AdminHash string   `json:"admin_hash"`
	Users     []User   `json:"users"`
	Repos     []string `json:"repos"`
	Paused    bool     `json:"paused"`
	// Owners is kept across restarts: daemon ids are public, and one bound
	// only in memory would go to whoever asked first after a restart. A file
	// written before it existed has none, and still loads.
	Owners map[core.DaemonID]string `json:"owners,omitempty"`
	// Members maps an allowed repo to the only persons who see it, sorted. A
	// repo without an entry is every participant's, as in a file written
	// before members existed.
	Members map[string][]string `json:"members,omitempty"`
	// Invites are the codes not spent yet, each for one person's token. A
	// file written before invites existed has none, and still loads.
	Invites []Invite `json:"invites,omitempty"`
}

// User is one person's token, stored as its sha256 hex only.
type User struct {
	Name      string `json:"name"`
	TokenHash string `json:"token_hash"`
	Revoked   bool   `json:"revoked"`
}

// writeFile is a seam so tests can simulate a crash before the rename.
var writeFile = defaultWriteFile
var defaultWriteFile = statedir.WriteAtomic

// LoadState reads the state file. A missing file wraps fs.ErrNotExist.
func LoadState(path string) (Stored, error) {
	var s Stored
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return Stored{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.Version != stateVersion {
		return Stored{}, fmt.Errorf("%s: version %d not supported (expected %d)", path, s.Version, stateVersion)
	}
	return s, nil
}

// SaveState replaces the state file atomically with mode 0600. Empty lists
// go as [], not null.
func SaveState(path string, s Stored) error {
	if s.Users == nil {
		s.Users = []User{}
	}
	if s.Repos == nil {
		s.Repos = []string{}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(b, '\n'))
}

// NewToken returns a fresh token and its stored hash. The token is shown once
// and never logged.
func NewToken() (token, hash string, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	token = tokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	return token, HashToken(token), nil
}

// HashToken is the sha256 of the token in lowercase hex.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
