// Package mcp is `dugout mcp`, the MCP server the plugin starts: JSON-RPC 2.0
// over stdin and stdout, one message per line, with the tools the agent calls
// to declare its intent, to ask who else has a file in flight or works in the
// repo, to record who keeps the span of a conflict, and to take to both humans
// what the agents cannot settle. Every tool asks the daemon, which a Daemon
// reaches.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/daemon"
	"github.com/joacofg/dugout/internal/wire"
)

// CallTimeout bounds one call to the daemon, so a tool answers within a
// second even when the daemon does not.
const CallTimeout = 900 * time.Millisecond

// EscalateTimeout bounds office_escalate's call, which waits for this
// machine's desktop notification and for serve to hand it to the other side.
const EscalateTimeout = 5 * time.Second

// Daemon is how the server reaches the daemon: one request, one response.
type Daemon interface {
	Call(ctx context.Context, req wire.Request) (wire.Response, error)
}

const (
	// latestProtocol is answered to a client that asks for a version not in
	// protocols.
	latestProtocol = "2025-06-18"
	// maxLine caps one incoming message; a longer one ends Serve.
	maxLine = 8 << 20

	// JSON-RPC 2.0 error codes.
	codeParse          = -32700
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602

	toolDeclare  = "office_declare_intent"
	toolWho      = "office_who_touches"
	toolOffice   = "office_who"
	toolClaim    = "office_claim"
	toolYield    = "office_yield"
	toolEscalate = "office_escalate"

	daemonDown     = "The Dugout daemon is not responding: run dugout doctor."
	declared       = "Intent recorded."
	untracked      = "That file is not in a repo Dugout tracks."
	nobody         = "Nobody else has that file in flight."
	missingSummary = "missing argument summary: a sentence about the task, up to 280 characters."
	missingSession = "missing argument session_id: the one Dugout gave you when the session started."
	missingPath    = "missing argument path."
	missingID      = "missing argument conflict_id: the one shown in the Dugout warning or office_who."
	missingReason  = "missing argument reason: what the humans have to decide and what each side wants, up to 280 characters."
	notInRepo      = "This directory is not in a repo Dugout tracks."
	alone          = "Nobody else is working in this repo right now."
	officeHint     = "Dugout: write to a session named above with SendMessage (to: its session name); the message reaches it even if it already finished. Who keeps a conflicting span is recorded with office_claim or office_yield."
	serverDown     = "(I could not ask the server: this is only what is on this machine.)"
	// The pause's notices, as the hooks give them.
	pausedTeam  = "Dugout is paused for the whole team."
	pausedLocal = "Dugout is paused on this machine."
)

// protocols are the MCP versions this server speaks, the newest first.
var protocols = []string{latestProtocol, "2025-03-26", "2024-11-05"}

type tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema schema `json:"inputSchema"`
}

type schema struct {
	Type       string          `json:"type"`
	Properties map[string]prop `json:"properties"`
	Required   []string        `json:"required"`
}

type prop struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

var tools = []tool{
	{
		Name:        toolDeclare,
		Description: "Tell Dugout what you are doing, in one sentence about the task (not the user's prompt). The agents of other people who touch the same files see it.",
		InputSchema: schema{Type: "object", Properties: map[string]prop{
			"session_id": {"string", "The session_id Dugout gave you when the session started."},
			"summary":    {"string", "One sentence about the task, up to 280 characters."},
		}, Required: []string{"session_id", "summary"}},
	},
	{
		Name:        toolWho,
		Description: "Who else on the team has a file in flight: branch, daemon and intent.",
		InputSchema: schema{Type: "object", Properties: map[string]prop{
			"path": {"string", "Path of the file, relative to the working directory or absolute."},
		}, Required: []string{"path"}},
	},
	{
		Name:        toolOffice,
		Description: "Who else is working in this repo: each workstream in flight with its branch, intent and files, the Claude Code session you write to with SendMessage, who keeps each span in conflict with your branch, and which may be doing the same task as yours.",
		InputSchema: schema{Type: "object", Properties: map[string]prop{}, Required: []string{}},
	},
	{
		Name:        toolClaim,
		Description: "Record that your branch keeps the span of a conflict, after agreeing it with the other session. Both sides see it in their next warning and in office_who; if both branches claim it, Dugout's fixed rule decides.",
		InputSchema: schema{Type: "object", Properties: map[string]prop{
			"session_id":  {"string", "The session_id Dugout gave you when the session started."},
			"conflict_id": {"string", "The id of the conflict, as the Dugout warning or office_who shows it."},
		}, Required: []string{"session_id", "conflict_id"}},
	},
	{
		Name: toolEscalate,
		Description: "Take to both humans, yours and the other branch's, what you cannot settle with the other agent over a conflict: contradictory tasks, a yield that would leave part of your task undone, or a change to a shared contract. " +
			"Both get a desktop notification naming each side. One question at a time: after it, leave that span as it is and wait for your human's answer in this session. An agreed yield is not an escalation: record it with office_yield.",
		InputSchema: schema{Type: "object", Properties: map[string]prop{
			"session_id":  {"string", "The session_id Dugout gave you when the session started."},
			"reason":      {"string", "What the humans have to decide and what each side wants, e.g. \"A keeps the 500 cap, B raises it to 2000: which stays?\". Up to 280 characters."},
			"urgency":     {"string", "normal (the default) or urgent: urgent rings."},
			"conflict_id": {"string", "The id of the conflict, as the Dugout warning or office_who shows it; optional when your branch has only one."},
		}, Required: []string{"session_id", "reason"}},
	},
	{
		Name:        toolYield,
		Description: "Record that you yield the span of a conflict to the other branch: you align your change to theirs or take that span out of yours. Both sides see it in their next warning and in office_who; if both branches yield it, Dugout's fixed rule decides.",
		InputSchema: schema{Type: "object", Properties: map[string]prop{
			"session_id":  {"string", "The session_id Dugout gave you when the session started."},
			"conflict_id": {"string", "The id of the conflict, as the Dugout warning or office_who shows it."},
		}, Required: []string{"session_id", "conflict_id"}},
	},
}

type request struct {
	ID     json.RawMessage `json:"id"` // nil when absent: a notification
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // null when the request could not be read
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

func textResult(text string, isError bool) toolResult {
	return toolResult{Content: []content{{Type: "text", Text: text}}, IsError: isError}
}

type server struct {
	ctx        context.Context
	d          Daemon
	cwd        string
	version    string
	timeout    time.Duration // per call to the daemon; Serve sets CallTimeout
	escTimeout time.Duration // per office_escalate; Serve sets EscalateTimeout
}

// Serve answers the JSON-RPC requests on in, one per line, on out, until in
// ends (nil) or ctx does (its error). A line that is not JSON gets a parse
// error and the loop goes on; a notification gets nothing. Relative paths in
// tool calls are read against cwd. Requests are answered one at a time.
func Serve(ctx context.Context, in io.Reader, out io.Writer, d Daemon, cwd, version string) error {
	return serveWith(ctx, in, out, d, cwd, version, CallTimeout, EscalateTimeout)
}

// serveWith is Serve with its own per-call timeout, which tests shorten.
func serveWith(ctx context.Context, in io.Reader, out io.Writer, d Daemon, cwd, version string, timeout, escalate time.Duration) error {
	s := &server{ctx: ctx, d: d, cwd: cwd, version: version, timeout: timeout, escTimeout: escalate}
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	// The reader runs apart so ctx can end Serve during a blocked read.
	go func() {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 64<<10), maxLine)
		for sc.Scan() {
			select {
			case lines <- slices.Clone(sc.Bytes()):
			case <-ctx.Done():
				return
			}
		}
		readErr <- sc.Err()
	}()
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case line := <-lines:
			if resp := s.handle(line); resp != nil {
				if err := enc.Encode(resp); err != nil {
					return err
				}
			}
		}
	}
}

// handle answers one line, or returns nil for a notification or a blank line.
func (s *server) handle(line []byte) *response {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return &response{JSONRPC: "2.0", Error: &rpcError{Code: codeParse, Message: "invalid JSON"}}
	}
	if req.ID == nil {
		return nil
	}
	resp := &response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = s.initialize(req.Params)
	case "ping":
		resp.Result = struct{}{}
	case "tools/list":
		resp.Result = map[string]any{"tools": tools}
	case "tools/call":
		resp.Result, resp.Error = s.callTool(req.Params)
	default:
		resp.Error = &rpcError{Code: codeMethodNotFound, Message: "unknown method: " + req.Method}
	}
	return resp
}

// initialize answers the client's protocol version when this server speaks
// it, and the latest otherwise.
func (s *server) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	version := latestProtocol
	if slices.Contains(protocols, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]string{"name": "dugout", "version": s.version},
	}
}

func (s *server) callTool(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid parameters"}
	}
	if !slices.ContainsFunc(tools, func(t tool) bool { return t.Name == p.Name }) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
	}
	var args struct {
		SessionID  string `json:"session_id"`
		Summary    string `json:"summary"`
		Resumen    string `json:"resumen"` // legacy name of summary: a client with a cached schema still sends it
		Intent     string `json:"intent"`  // what agents send for summary at first, unasked
		Path       string `json:"path"`
		ConflictID string `json:"conflict_id"`
		Reason     string `json:"reason"`
		Urgency    string `json:"urgency"`
	}
	if len(p.Arguments) > 0 {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid arguments for " + p.Name}
		}
	}
	// A missing argument is named, so the agent can fix its call; the daemon
	// is not asked. "intent" stands in for "summary": real agents sent it
	// first and paid one more call and one more permission for it.
	switch p.Name {
	case toolOffice:
		return s.office(), nil
	case toolEscalate:
		switch {
		case strings.TrimSpace(args.SessionID) == "":
			return textResult(missingSession, true), nil
		case strings.TrimSpace(args.Reason) == "":
			return textResult(missingReason, true), nil
		}
		return s.escalate(args.SessionID, args.Reason, args.Urgency, args.ConflictID), nil
	case toolClaim, toolYield:
		switch {
		case strings.TrimSpace(args.SessionID) == "":
			return textResult(missingSession, true), nil
		case strings.TrimSpace(args.ConflictID) == "":
			return textResult(missingID, true), nil
		}
		typ := wire.TypeClaim
		if p.Name == toolYield {
			typ = wire.TypeYield
		}
		return s.stance(typ, args.SessionID, args.ConflictID), nil
	}
	if p.Name == toolDeclare {
		if strings.TrimSpace(args.Summary) == "" {
			args.Summary = args.Resumen
		}
		if strings.TrimSpace(args.Summary) == "" {
			args.Summary = args.Intent
		}
		switch {
		case strings.TrimSpace(args.Summary) == "":
			return textResult(missingSummary, true), nil
		case strings.TrimSpace(args.SessionID) == "":
			return textResult(missingSession, true), nil
		}
		return s.declare(args.SessionID, args.Summary), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return textResult(missingPath, true), nil
	}
	return s.who(args.Path), nil
}

// call asks the daemon within s.timeout, even when d does not heed its
// context: then its answer is dropped when it comes.
func (s *server) call(req wire.Request) (wire.Response, error) {
	return s.callWithin(req, s.timeout)
}

// callWithin is call within timeout.
func (s *server) callWithin(req wire.Request, timeout time.Duration) (wire.Response, error) {
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()
	type result struct {
		resp wire.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := s.d.Call(ctx, req)
		done <- result{resp, err}
	}()
	select {
	case r := <-done:
		return r.resp, r.err
	case <-ctx.Done():
		return wire.Response{}, ctx.Err()
	}
}

// declare hands the intent to the daemon, which sanitizes it and refuses a
// session it did not register; its refusal goes to the agent as is.
func (s *server) declare(session, summary string) toolResult {
	resp, err := s.call(wire.Request{Type: wire.TypeIntent, SessionID: session, Text: summary})
	switch {
	case err != nil:
		return textResult(daemonDown, true)
	case resp.Error != "":
		return textResult(resp.Error, true)
	}
	return textResult(declared, false)
}

// who asks the daemon about path, read against cwd when relative. The other
// workstreams go inside the context block, sanitized; while Dugout is
// paused, only the pause's notice goes.
func (s *server) who(path string) toolResult {
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.cwd, path)
	}
	resp, err := s.call(wire.Request{Type: wire.TypeWho, Path: path})
	if err != nil {
		return textResult(daemonDown, true)
	}
	if resp.Error != "" {
		return textResult(resp.Error, true)
	}
	var w wire.Who
	if err := json.Unmarshal(resp.Output, &w); err != nil {
		return textResult(daemonDown, true)
	}
	switch {
	case w.Paused == "team":
		return textResult(pausedTeam, false)
	case w.Paused != "":
		return textResult(pausedLocal, false)
	case !w.Tracked:
		return textResult(untracked, false)
	}
	text := nobody
	if len(w.Touches) > 0 {
		rows := make([]string, 0, len(w.Touches))
		for _, t := range w.Touches {
			row := fmt.Sprintf(`- branch "%s" (daemon %s)`, daemon.Sanitize(t.Branch), short(daemon.Sanitize(string(t.Daemon))))
			if intent := daemon.SanitizeIntent(t.Intent); intent != "" {
				row += fmt.Sprintf(`: "%s"`, intent)
			}
			rows = append(rows, row)
		}
		text = daemon.Block(rows...)
	}
	if w.Server == "down" {
		text += "\n" + serverDown
	}
	return textResult(text, false)
}

// office asks the daemon who works in the repo of the server's directory. The
// rows go inside the context block, sanitized again, and Dugout's
// word on the channel after it; while Dugout is paused, only the pause's
// notice goes.
func (s *server) office() toolResult {
	resp, err := s.call(wire.Request{Type: wire.TypeOffice, Path: s.cwd})
	if err != nil {
		return textResult(daemonDown, true)
	}
	if resp.Error != "" {
		return textResult(resp.Error, true)
	}
	var o wire.Office
	if err := json.Unmarshal(resp.Output, &o); err != nil {
		return textResult(daemonDown, true)
	}
	switch {
	case o.Paused == "team":
		return textResult(pausedTeam, false)
	case o.Paused != "":
		return textResult(pausedLocal, false)
	case !o.Tracked:
		return textResult(notInRepo, false)
	}
	text := alone
	if len(o.Workstreams) > 0 {
		var rows []string
		named := false
		for _, w := range o.Workstreams {
			rows = append(rows, officeRow(w)...)
			named = named || w.Session != ""
		}
		text = daemon.Block(rows...)
		if named {
			text += "\n" + officeHint
		}
	}
	if o.Server == "down" {
		text += "\n" + serverDown
	}
	return textResult(text, false)
}

// officeRow is w's line in office_who, and one under it per conflict with the
// asking worktree and one when it may duplicate the asking worktree's task.
func officeRow(w wire.OfficeWorkstream) []string {
	where := ", no session open on this machine"
	switch {
	case w.Remote:
		where = ", on another machine: no channel to it yet"
	case w.Session != "":
		where = fmt.Sprintf(`, session "%s"`, daemon.Sanitize(w.Session))
	}
	row := fmt.Sprintf(`- branch "%s" (daemon %s%s)`, daemon.Sanitize(w.Branch), short(daemon.Sanitize(w.Daemon)), where)
	if intent := daemon.SanitizeIntent(w.Intent); intent != "" {
		row += fmt.Sprintf(`: "%s"`, intent)
	}
	files := make([]string, 0, len(w.Files)+1)
	for _, f := range w.Files {
		files = append(files, `"`+daemon.Sanitize(f)+`"`)
	}
	if w.FilesMore > 0 {
		files = append(files, fmt.Sprintf("%d more", w.FilesMore))
	}
	if len(files) == 0 {
		row += ". No files in flight yet."
	} else {
		row += ". In flight: " + enumerate(files) + "."
	}
	out := []string{row}
	for _, c := range w.Conflicts {
		out = append(out, fmt.Sprintf(`  conflict %s in "%s": %s.`, daemon.Sanitize(c.ID), daemon.Sanitize(c.Path), keeps(c.Keeps, c.Why)))
	}
	if dup := w.Duplicate; dup != nil {
		line := fmt.Sprintf(`  possible duplicate %s: its intent looks like the same task as yours.`, daemon.Sanitize(dup.ID))
		if why := daemon.Sanitize(dup.Reason); why != "" {
			line += " Why: " + why
		}
		out = append(out, line)
	}
	return out
}

// keeps says who keeps a span, from the asking side, and how when the
// registry decided it.
func keeps(mine bool, why string) string {
	who := "your branch yields"
	if mine {
		who = "your branch keeps it"
	}
	if why = daemon.Sanitize(why); why != "" {
		return who + " (" + why + ")"
	}
	return who
}

// stance hands a claim or a yield to the daemon, which refuses a session it
// did not register, a conflict it does not see and one with another
// machine; its refusal goes to the agent as is.
func (s *server) stance(typ, session, conflict string) toolResult {
	resp, err := s.call(wire.Request{Type: typ, SessionID: session, Conflict: conflict})
	switch {
	case err != nil:
		return textResult(daemonDown, true)
	case resp.Error != "":
		return textResult(resp.Error, true)
	}
	var st wire.Stance
	if err := json.Unmarshal(resp.Output, &st); err != nil {
		return textResult(daemonDown, true)
	}
	text := fmt.Sprintf(`Recorded. Conflict %s in "%s": %s.`, daemon.Sanitize(st.ID), daemon.Sanitize(st.Path), keeps(st.Keeps, st.Why))
	if st.Resolved {
		text = fmt.Sprintf(`Recorded. Conflict %s in "%s" no longer conflicts; if it comes back, %s.`, daemon.Sanitize(st.ID), daemon.Sanitize(st.Path), keeps(st.Keeps, st.Why))
	} else if st.Advice != "" {
		// The advice is Dugout's own text, the mode's, as the daemon's refusals
		// are: Sanitize's 200-rune cap is for other workstreams' fields and
		// would cut auto mode's escalation triggers.
		text += " " + st.Advice
		if len(st.Text) > 0 {
			text += "\n" + daemon.Block(st.Text...)
		}
	}
	if st.Session != "" {
		text += fmt.Sprintf(` Tell session "%s" with SendMessage: Dugout tells it in its next warning, but if it already finished it will not see it until you write to it.`, daemon.Sanitize(st.Session))
	} else {
		text += fmt.Sprintf(` Branch "%s" has no open session on this machine: it sees this when it opens one.`, daemon.Sanitize(st.Branch))
	}
	return textResult(text, false)
}

// escalate hands office_escalate to the daemon, which refuses a session it
// did not register, a second question before the first was answered, and
// a conflict it does not see; its refusal goes to the agent as is. The answer
// says who was told and how far it got: a desktop that could not show it, or
// another machine it did not reach, is never left unsaid, and the agent is
// told to ask its human in its own session instead.
func (s *server) escalate(session, reason, urgency, conflict string) toolResult {
	resp, err := s.callWithin(wire.Request{Type: wire.TypeEscalate, SessionID: session, Text: reason, Urgency: urgency, Conflict: conflict}, s.escTimeout)
	switch {
	case err != nil:
		return textResult(daemonDown+" Nothing was escalated: ask your human in this session.", true)
	case resp.Error != "":
		return textResult(resp.Error, true)
	}
	var e wire.Escalated
	if err := json.Unmarshal(resp.Output, &e); err != nil {
		return textResult(daemonDown, true)
	}
	// Mine and Theirs are the daemon's own text, built of sanitized fields, as
	// a stance's advice is: Sanitize would turn their quotes.
	mine, theirs := e.Mine, e.Theirs
	lines := []string{fmt.Sprintf(`Escalated conflict %s in "%s" to both humans: %s and %s.`, daemon.Sanitize(e.ID), daemon.Sanitize(e.Path), mine, theirs)}
	if e.Desktop == "" {
		lines = append(lines, "Your human got a desktop notification on this machine.")
	} else {
		lines = append(lines, fmt.Sprintf("This machine could not show your human a desktop notification (%s): ask them in this session now, in one line, what they have to decide.", daemon.Sanitize(e.Desktop)))
	}
	switch e.Other {
	case "local":
		if e.Session != "" {
			lines = append(lines, fmt.Sprintf(`The other side works on this machine: session "%s" hears of it in its next Dugout answer; write to it with SendMessage if it already finished.`, daemon.Sanitize(e.Session)))
		} else {
			lines = append(lines, "The other side works on this machine, with no session open: it hears of it when it opens one.")
		}
	case "relayed":
		lines = append(lines, "It reached the other side's machine through the server: their desktop shows it and their agent hears of it at its next turn.")
	default:
		lines = append(lines, fmt.Sprintf("It did not reach the other side's machine (%s): ask your human to tell them.", daemon.Sanitize(e.OtherWhy)))
	}
	lines = append(lines, "Leave that span as it is until your human answers you here, and go on with the rest of your task. One question at a time: your next office_escalate waits for that answer.")
	return textResult(strings.Join(lines, "\n"), false)
}

// enumerate joins items as "a, b and c".
func enumerate(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// short is the first 8 runes of s, as the daemon names another daemon.
func short(s string) string {
	if r := []rune(s); len(r) > 8 {
		return string(r[:8])
	}
	return s
}
