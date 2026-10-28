package serve

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"path"
	"slices"
	"time"
)

const (
	invitePrefix = "dgi_"
	// DefaultInviteTTL is how long an invite lasts when the admin sets none:
	// a day to paste it, short enough that a code left in a chat goes stale.
	DefaultInviteTTL = 24 * time.Hour
	// MaxInviteTTL bounds what the admin may ask for.
	MaxInviteTTL = 7 * 24 * time.Hour
	minInviteTTL = time.Minute
	// joinPath is the one route that takes no token: the invite code in its
	// body is the credential.
	joinPath = "/v1/join"
)

// Invite is a one-time code that redeems for Name's token until Expires,
// stored as its sha256 hex only, like a token.
type Invite struct {
	Name     string    `json:"name"`
	CodeHash string    `json:"code_hash"`
	Expires  time.Time `json:"expires"`
}

// NewInviteCode returns a fresh invite code and its stored hash. The code
// is shown once, to the admin, and never logged.
func NewInviteCode() (code, hash string, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	code = invitePrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	return code, HashToken(code), nil
}

// isJoinPath reports whether p is the join route, cleaned as the mux cleans it.
func isJoinPath(p string) bool { return path.Clean(p) == joinPath }

// liveInvites drops the invites expired at now.
func liveInvites(in []Invite, now time.Time) []Invite {
	return slices.DeleteFunc(in, func(i Invite) bool { return !now.Before(i.Expires) })
}

// handleInviteCreate answers the only response that carries an invite code:
// it goes to no log and no error. An invite replaces any pending one for the
// same person, and none is made for someone who already has a token.
func (s *Server) handleInviteCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		TTLSeconds int64  `json:"ttl_seconds"`
	}
	if !decodeAdmin(w, r, &req) {
		return
	}
	name, bad := personName(req.Name)
	if bad != nil {
		http.Error(w, bad.msg, bad.code)
		return
	}
	ttl := DefaultInviteTTL
	if req.TTLSeconds != 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	if ttl < minInviteTTL || ttl > MaxInviteTTL {
		http.Error(w, "invalid expiry: from 1 minute to 7 days", http.StatusBadRequest)
		return
	}
	code, hash, err := NewInviteCode()
	if err != nil {
		http.Error(w, "could not generate the invite", http.StatusInternalServerError)
		return
	}
	expires := s.clock().Add(ttl).UTC().Truncate(time.Second)
	if !s.change(w, func(st *Stored) *refusal {
		if slices.ContainsFunc(st.Users, func(u User) bool { return u.Name == name && !u.Revoked }) {
			return &refusal{http.StatusConflict, "there is already a token for " + name + ": revoke it first to invite them again"}
		}
		st.Invites = slices.DeleteFunc(liveInvites(st.Invites, s.clock()), func(i Invite) bool { return i.Name == name })
		st.Invites = append(st.Invites, Invite{Name: name, CodeHash: hash, Expires: expires})
		return nil
	}, nil) {
		return
	}
	s.logger().Info("invite created", "name", name, "expires", expires)
	writeJSON(w, http.StatusCreated, struct {
		Name    string    `json:"name"`
		Code    string    `json:"code"`
		Expires time.Time `json:"expires"`
	}{name, code, expires})
}

// handleJoin redeems an invite code for its person's token, as
// handleTokenIssue issues one, and spends the code in the same save. An
// unknown, spent or expired code gets the same answer, so it tells nothing.
func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decodeAdmin(w, r, &req) {
		return
	}
	token, tokenHash, err := NewToken()
	if err != nil {
		http.Error(w, "could not generate the token", http.StatusInternalServerError)
		return
	}
	hash := []byte(HashToken(req.Code))
	name := ""
	if !s.change(w, func(st *Stored) *refusal {
		now := s.clock()
		found := -1
		for i, inv := range st.Invites {
			if subtle.ConstantTimeCompare(hash, []byte(inv.CodeHash)) == 1 && now.Before(inv.Expires) {
				found = i
			}
		}
		if req.Code == "" || found < 0 {
			return &refusal{http.StatusUnauthorized, "invalid invite: it was already used, it expired or it never existed"}
		}
		name = st.Invites[found].Name
		if slices.ContainsFunc(st.Users, func(u User) bool { return u.Name == name && !u.Revoked }) {
			return &refusal{http.StatusConflict, "there is already a token for " + name + ": ask the admin to revoke it and invite you again"}
		}
		st.Invites = liveInvites(slices.Delete(st.Invites, found, found+1), now)
		st.Users = append(st.Users, User{Name: name, TokenHash: tokenHash})
		return nil
	}, nil) {
		s.logger().Info("invite refused")
		return
	}
	s.logger().Info("token issued", "name", name, "via", "invite")
	writeJSON(w, http.StatusCreated, struct {
		Name  string `json:"name"`
		Token string `json:"token"`
	}{name, token})
}
