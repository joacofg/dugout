// Package wire holds the messages of Dugout's two protocols: hook, track,
// intent, who, office, claim, yield and escalate requests to the daemon over the unix
// socket (one JSON line each way per connection), and daemon messages to
// serve over HTTP. Every
// message carries a type; receivers ignore types they do not know.
package wire

import (
	"encoding/json"
	"time"

	"github.com/joacofg/dugout/internal/core"
)

// Request types on the daemon socket.
const (
	TypeHook  = "hook"
	TypeTrack = "track"
	// TypeIntent declares what a session's agent is doing: SessionID
	// and Text.
	TypeIntent = "intent"
	// TypeWho asks which other workstreams have Path, absolute, in flight;
	// the answer's Output is a Who.
	TypeWho = "who"
	// TypeStatus asks what the daemon sees on this machine, for `dugout
	// status`; the answer's Output is a Status.
	TypeStatus = "status"
	// TypeMode reads, or with Text sets, the autonomy mode of the repo that
	// holds Path, absolute (`dugout mode`, `dugout init`, /dugout). The
	// answer is a Mode.
	TypeMode = "mode"
	// TypeOffice asks who works in the repo of Path, absolute: every other
	// workstream in flight and the session to message on each. The answer's
	// Output is an Office.
	TypeOffice = "office"
	// TypeClaim and TypeYield tell the office registry that SessionID's
	// workstream keeps, or gives the other side, the span of Conflict (an ID,
	// or a prefix of one, as the notices show it). The answer's Output is a
	// Stance.
	TypeClaim = "claim"
	TypeYield = "yield"
	// TypeEscalate is office_escalate: SessionID's agent takes to both humans
	// what it cannot settle with the other side of Conflict (an ID or a
	// prefix of one; "" for the session's only conflict), Text saying what to
	// decide, with Urgency. The answer's Output is an Escalated.
	TypeEscalate = "escalate"
)

// Message types from the daemon to serve.
const (
	TypeFullState = "full_state"
	// TypeConflicts carries the textual conflicts a daemon found.
	// An old serve ignores it.
	TypeConflicts = "conflicts"
	// TypeDelivered is reserved for "the hook delivered warning id at t".
	// Nothing sends it yet.
	TypeDelivered = "delivered"
	// TypeEscalation carries an Escalation for serve to hand to the other
	// side's daemon on its stream; serve answers a Relayed. An old serve
	// ignores it with 204, which tells the sender it went nowhere.
	TypeEscalation = "escalation"
	// TypeOutcomes carries the records of conflicts that ended on the
	// sending daemon: counts and identities, never code or a
	// path. An old serve ignores it with 204.
	TypeOutcomes = "outcomes"
	// TypeAgentMessage says a session of one of the sender's workstreams
	// messaged a session of another of its workstreams (SendMessage), for the
	// office view: who and when, never the text, which the daemon never sees.
	// An old serve ignores it with 204.
	TypeAgentMessage = "message"
	// TypeEscalated tells serve of an escalation the sender raised, by an
	// agent or by a rule, for the office view. An old serve ignores it with
	// 204.
	TypeEscalated = "escalated"
)

// EnvToken holds the token shared by serve and the daemons. It
// travels as "Authorization: Bearer <token>". It is read from the
// environment, never from a flag, so it does not show up in ps.
const EnvToken = "DUGOUT_TOKEN"

// ProtocolVersion is the wire protocol this build speaks; serve answers it in
// HeaderProtocol. A missing header means an older build (version 0). Version
// 2 brought ratings of the warnings, since dropped: serve answers a "rating"
// an older daemon still sends 204 and ignores it, as any type it does not
// know.
const (
	HeaderProtocol  = "X-Dugout-Protocol"
	ProtocolVersion = 2
	// MinProtocol is the oldest daemon protocol serve accepts; below it serve
	// answers 426. A daemon that sends none (0) predates the field and is
	// still accepted.
	MinProtocol = 1
)

// StreamKeepAlive is how often serve writes a comment line on an idle
// warnings stream, so dead connections surface on both ends.
const StreamKeepAlive = 15 * time.Second

// Snapshot bundle relay (POST and GET /v1/snapshot).
const (
	BundleType   = "application/x-git-bundle"
	HeaderCommit = "X-Dugout-Commit"
	HeaderBase   = "X-Dugout-Base"
	MaxBundle    = 20 << 20
)

// Request is what `dugout hook`, `dugout track` and the MCP server send to
// the daemon.
type Request struct {
	Type      string          `json:"type"`
	Event     string          `json:"event,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"` // hook stdin, without the user's prompt
	ClaudePID int             `json:"claude_pid,omitempty"`
	Path      string          `json:"path,omitempty"`       // what track tracks, or the file a who asks about
	SessionID string          `json:"session_id,omitempty"` // the session an intent, claim or yield is for
	Text      string          `json:"text,omitempty"`       // the intent, never the user's prompt; or the mode a mode request sets
	// ClaudeDir is the Claude Code config dir of a hook's session, whose
	// registry names the session (package peers).
	ClaudeDir string `json:"claude_dir,omitempty"`
	Conflict  string `json:"conflict,omitempty"` // the conflict a claim, yield or escalate is about
	Urgency   string `json:"urgency,omitempty"`  // an escalate's: "normal" or "urgent"
}

// Response is the daemon's answer. Output is the hook's stdout JSON.
// Terminal, for a hook, is a line the hook writes to its terminal as an OSC 9
// notification: an escalation this machine's desktop could not show. An old
// hook ignores it.
type Response struct {
	Output   json.RawMessage `json:"output,omitempty"`
	Error    string          `json:"error,omitempty"`
	Terminal string          `json:"terminal,omitempty"`
}

// Message is one daemon-to-serve message (POST /v1/msg).
type Message struct {
	Type string `json:"type"`
	core.FullState
	Conflicts []core.Conflict `json:"conflicts,omitempty"`
	// Escalation is a TypeEscalation's.
	Escalation *Escalation `json:"escalation,omitempty"`
	// Outcomes is a TypeOutcomes's.
	Outcomes []core.Record `json:"outcomes,omitempty"`
	// Stances is a TypeConflicts's: what each side of a conflict told the
	// sender's registry, by conflict ID, in the conflict's side order; only
	// the conflicts with a stance. An old serve ignores it.
	Stances map[string][2]core.Stance `json:"stances,omitempty"`
	// AgentMessage is a TypeAgentMessage's.
	AgentMessage *AgentMessage `json:"message,omitempty"`
	// Escalated is a TypeEscalated's.
	Escalated *EscalationNotice `json:"escalated,omitempty"`
	// ProtocolVersion is the sender's ProtocolVersion. An older daemon sends
	// none, which reads as 0.
	ProtocolVersion int `json:"protocol_version,omitempty"`
}

// Control is the "control" event of the warnings stream: what serve tells
// every daemon besides warnings. InjectionOff is the team-wide pause; while
// it is set the daemons answer every hook with nothing. An old serve never
// sends it, which reads as false.
type Control struct {
	InjectionOff bool `json:"injection_off"`
}

// StateDump is GET /v1/state.
type StateDump struct {
	Workstreams []core.Workstream `json:"workstreams"`
	Warnings    []core.Warning    `json:"warnings"`
	Conflicts   []core.Conflict   `json:"conflicts"`
}

// Who answers a TypeWho request. A path outside every tracked worktree
// answers only {"tracked":false}.
type Who struct {
	Tracked bool `json:"tracked"`
	// Paused is "team" or "local" while Dugout is paused, the team's
	// first: then the answer is only {"tracked":true,"paused":...}, whatever
	// the path.
	Paused string `json:"paused,omitempty"`
	// Server is "ok" when serve answered, "down" when it did not and "off"
	// without an uplink; but for "ok", Touches are only this machine's.
	Server string `json:"server"`
	// Touches are the other workstreams of the repo with the file in flight,
	// sorted by workstream ID, as their daemons sent them: not sanitized.
	Touches []Touch `json:"touches"`
}

// Touch is one other workstream in a Who.
type Touch struct {
	Branch string        `json:"branch"`
	Daemon core.DaemonID `json:"daemon"`
	Intent string        `json:"intent"`
}

// Mode answers a TypeMode request: the repo's mode after it, "" when never
// chosen.
type Mode struct {
	Repo string `json:"repo"`
	Mode string `json:"mode"`
}

// Status answers a TypeStatus request: this machine's tracked worktrees with
// files in flight, sorted by root, and what touches each. Every text is
// sanitized by the daemon.
type Status struct {
	Daemon    string `json:"daemon"`
	Server    string `json:"server"` // "" without an uplink
	Connected bool   `json:"connected"`
	Paused    string `json:"paused"` // "local", "team" or ""
	// Judge is the duplicate detector's judge. An older daemon sends none,
	// which reads as off with no reason.
	Judge StatusJudge `json:"judge"`
	// Workstreams is never nil.
	Workstreams []StatusWorkstream `json:"workstreams"`
	// Ended is the conflicts that ended on this machine since the daemon
	// started, the latest first, at most a few; Totals adds up all of them
	// by repo, sorted. Neither is ever nil.
	Ended  []StatusEnded  `json:"ended"`
	Totals []StatusTotals `json:"totals"`
}

// StatusEnded is a conflict that ended, with what was measured of it.
// Branches are its sides', in its order.
type StatusEnded struct {
	Repo     string    `json:"repo"`
	Path     string    `json:"path"`
	Branches [2]string `json:"branches"`
	// Outcome is a core.Outcome: resolved, committed or gone.
	Outcome string     `json:"outcome"`
	Tally   core.Tally `json:"tally"`
}

// StatusTotals adds up a repo's ended conflicts.
type StatusTotals struct {
	Repo string `json:"repo"`
	core.Totals
}

// StatusJudge is the judge that compares declared intents for duplicate
// work, and what it has cost since the daemon started. Off says why there is
// none; then the rest is zero.
type StatusJudge struct {
	Off          string  `json:"off,omitempty"`
	Name         string  `json:"name,omitempty"` // which judge and model
	Comparisons  int     `json:"comparisons"`
	Duplicates   int     `json:"duplicates"`
	Errors       int     `json:"errors"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	USD          float64 `json:"usd"`
	Unpriced     int     `json:"unpriced,omitempty"` // comparisons of a model with no known price, outside USD
	LastError    string  `json:"last_error,omitempty"`
}

// StatusWorkstream is one worktree in a Status.
type StatusWorkstream struct {
	Root      string           `json:"root"`
	Repo      string           `json:"repo"`
	Branch    string           `json:"branch"`
	Files     []string         `json:"files"`
	Intent    string           `json:"intent"`
	Overlaps  []StatusOverlap  `json:"overlaps"`
	Conflicts []StatusConflict `json:"conflicts"`
	// Duplicates is never nil from a daemon that knows them.
	Duplicates []StatusDuplicate `json:"duplicates"`
}

// StatusDuplicate is another workstream whose declared intent the judge
// found to be the same task as this one's.
type StatusDuplicate struct {
	Branch string `json:"branch"`
	Daemon string `json:"daemon"`
	Intent string `json:"intent"`
	Reason string `json:"reason"`
}

// StatusOverlap is another workstream with a file of this one in flight.
type StatusOverlap struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Daemon string `json:"daemon"`
	Intent string `json:"intent"`
}

// StatusConflict is a textual conflict with another workstream; Lines is
// this side's lines, as "your lines ...", or "" for a file-level one.
type StatusConflict struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Daemon string `json:"daemon"`
	Lines  string `json:"lines"`
	Intent string `json:"intent"`
	// Tally is what was measured of it so far.
	Tally core.Tally `json:"tally"`
}

// Office answers a TypeOffice request: the repo's other workstreams, sorted
// by workstream ID. A path outside every tracked worktree answers only
// {"tracked":false}; while paused, {"tracked":true,"paused":...}. Every text
// is sanitized by the daemon.
type Office struct {
	Tracked bool   `json:"tracked"`
	Paused  string `json:"paused,omitempty"`
	// Server is as in Who: but for "ok", Workstreams are only this machine's.
	Server      string             `json:"server"`
	Branch      string             `json:"branch"` // the asking worktree's
	Workstreams []OfficeWorkstream `json:"workstreams"`
}

// OfficeWorkstream is one other workstream in an Office. Session is the name
// its agent's Claude Code session is messaged at, "" when it has none open on
// this machine; Remote, that it is another machine's, which no local channel
// reaches yet. Files are the first few in flight, FilesMore the rest.
type OfficeWorkstream struct {
	Branch    string           `json:"branch"`
	Daemon    string           `json:"daemon"`
	Intent    string           `json:"intent"`
	Session   string           `json:"session"`
	Remote    bool             `json:"remote,omitempty"`
	Files     []string         `json:"files"`
	FilesMore int              `json:"files_more,omitempty"`
	Conflicts []OfficeConflict `json:"conflicts"`
	// Duplicate is set when the judge found this workstream's intent to be
	// the same task as the asking one's.
	Duplicate *OfficeDuplicate `json:"duplicate,omitempty"`
}

// OfficeDuplicate is a possible duplicate of the asking worktree's task.
type OfficeDuplicate struct {
	ID     string `json:"id"` // the 8 characters the notices show
	Reason string `json:"reason"`
}

// OfficeConflict is a conflict of the asking worktree with an
// OfficeWorkstream, and who keeps its span.
type OfficeConflict struct {
	ID    string `json:"id"` // the 8 characters the notices show
	Path  string `json:"path"`
	Keeps bool   `json:"keeps"` // the asking worktree keeps it
	Why   string `json:"why"`   // how that was decided, as the notices say it
}

// Stance answers a TypeClaim or TypeYield: who keeps the span now, and the
// other side, to tell it.
type Stance struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Keeps bool   `json:"keeps"`
	Why   string `json:"why"`
	// Resolved: the conflict no longer stands; the stance holds if it comes
	// back.
	Resolved bool   `json:"resolved,omitempty"`
	Advice   string `json:"advice,omitempty"` // what to do where the caller yields, by its repo's mode
	Branch   string `json:"branch"`           // the other side's
	Session  string `json:"session"`          // the other side's session, "" when none
	// Text is the other side's lines of the conflict, sanitized and capped as
	// the notices show them.
	Text []string `json:"text,omitempty"`
}

// Escalation is an agent's office_escalate about a conflict between a
// workstream of its daemon (From) and one of another daemon's (To), on its
// way to To's human through serve. Serve takes it only from From's daemon
// and only for a pair it holds a warning on Path for, and sends on the sides
// as its own state has them. Every text is the asking side's: the daemon
// that shows it sanitizes it.
type Escalation struct {
	Conflict string    `json:"conflict"` // the conflict's ID
	Repo     string    `json:"repo"`
	Path     string    `json:"path"`
	From     core.Side `json:"from"`
	To       core.Side `json:"to"`
	Reason   string    `json:"reason"` // what to decide, in the asking agent's words
	Urgent   bool      `json:"urgent,omitempty"`
}

// Relayed answers a TypeEscalation: whether a stream of the other daemon took
// it. False is a daemon not connected to serve right now.
type Relayed struct {
	Delivered bool `json:"delivered"`
}

// Escalated answers a TypeEscalate: what went to the humans and how far it
// got. Every text is sanitized by the daemon.
type Escalated struct {
	ID     string `json:"id"` // the conflict's 8 characters
	Path   string `json:"path"`
	Mine   string `json:"mine"`   // how the escalation names the asking side
	Theirs string `json:"theirs"` // and the other side
	Text   string `json:"text"`   // what both humans were told
	// Desktop is "" when this machine showed the desktop notification, and
	// why it could not otherwise.
	Desktop string `json:"desktop,omitempty"`
	// Other is how it reached the other side: "local" (a workstream of this
	// machine, whose sessions hear it at their next hook), "relayed" (serve
	// handed it to the other daemon), or "unreached", with OtherWhy.
	Other    string `json:"other"`
	OtherWhy string `json:"other_why,omitempty"`
	Session  string `json:"session,omitempty"` // the other side's session on this machine
}

// AgentMessage is a TypeAgentMessage: a session of workstream From messaged
// one of workstream To, both the sender's.
type AgentMessage struct {
	From core.WorkstreamID `json:"from"`
	To   core.WorkstreamID `json:"to"`
}

// Triggers of an EscalationNotice: an agent's office_escalate, the time rule
// (a conflict still standing escalate_after after both sides were warned), or
// both sides claiming the span.
const (
	TriggerAgent    = "agent"
	TriggerTime     = "time"
	TriggerDisputed = "disputed"
)

// EscalationNotice is a TypeEscalated: the sender took Conflicts, all of them
// ones it reported, to both humans. From is the asking agent's workstream,
// Reason its words and Urgent its urgency; all three are empty for a rule's.
type EscalationNotice struct {
	Conflicts []string          `json:"conflicts"`
	Trigger   string            `json:"trigger"`
	From      core.WorkstreamID `json:"from,omitempty"`
	Reason    string            `json:"reason,omitempty"`
	Urgent    bool              `json:"urgent,omitempty"`
}

// ViewTokenPrefix starts a view token: one that only reads the office view
// (GET /v1/office and its stream), as the person who asked for it.
const ViewTokenPrefix = "dgv_"

// ViewToken answers POST /v1/office/token.
type ViewToken struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// OfficeView is GET /v1/office and each event of its stream: the office as
// the asking person may see it, one entry per repo, sorted by repo. Every
// text is as the daemons sent it, not sanitized: a client shows it as text.
type OfficeView struct {
	At    time.Time    `json:"at"`
	Repos []OfficeRepo `json:"repos"`
}

// OfficeRepo is one repo of an OfficeView. Every list is sorted and never nil.
type OfficeRepo struct {
	Repo        string             `json:"repo"`
	Workstreams []core.Workstream  `json:"workstreams"`
	Shared      []OfficeShared     `json:"shared"`
	Conflicts   []OfficeSpan       `json:"conflicts"`
	Messages    []OfficeMessage    `json:"messages"`    // oldest first
	Escalations []OfficeEscalation `json:"escalations"` // oldest first
}

// NewOfficeRepo is an OfficeRepo with nothing in it yet.
func NewOfficeRepo(repo string) OfficeRepo {
	return OfficeRepo{Repo: repo, Workstreams: []core.Workstream{}, Shared: []OfficeShared{}, Conflicts: []OfficeSpan{},
		Messages: []OfficeMessage{}, Escalations: []OfficeEscalation{}}
}

// OfficeShared is a file two workstreams both have in flight.
type OfficeShared struct {
	Path  string               `json:"path"`
	Sides [2]core.WorkstreamID `json:"sides"`
}

// OfficeSpan is a conflict and who keeps its span: Owner, decided as Why
// says.
type OfficeSpan struct {
	ID    string               `json:"id"`
	Path  string               `json:"path"`
	Kind  string               `json:"kind"`
	Sides [2]core.WorkstreamID `json:"sides"`
	Lines [2][]core.Range      `json:"lines"`
	Owner core.WorkstreamID    `json:"owner"`
	Why   core.OwnerWhy        `json:"why"`
}

// OfficeMessage is one message between two agents, stamped with serve's
// clock.
type OfficeMessage struct {
	At   time.Time         `json:"at"`
	From core.WorkstreamID `json:"from"`
	To   core.WorkstreamID `json:"to"`
}

// OfficeEscalation is one escalation, open while any of its conflicts stands.
// For an agent's, Sides[0] is the asking side.
type OfficeEscalation struct {
	At        time.Time            `json:"at"`
	Conflicts []string             `json:"conflicts"`
	Paths     []string             `json:"paths"`
	Sides     [2]core.WorkstreamID `json:"sides"`
	Trigger   string               `json:"trigger"`
	Reason    string               `json:"reason,omitempty"`
	Urgent    bool                 `json:"urgent,omitempty"`
	Open      bool                 `json:"open"`
}
