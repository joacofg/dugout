package wire

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/core"
)

// TestMessageRoundTripsNewFields: the office view's three facts travel in a
// Message and are absent from its JSON when unset, so older messages keep
// their exact shape.
func TestMessageRoundTripsNewFields(t *testing.T) {
	bare, err := json.Marshal(Message{Type: TypeFullState})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"stances"`, `"message"`, `"escalated"`} {
		if strings.Contains(string(bare), key) {
			t.Errorf("an unset %s is in %s", key, bare)
		}
	}
	in := Message{
		Type:         TypeAgentMessage,
		Stances:      map[string][2]core.Stance{"c1": {core.StanceClaim, core.StanceNone}},
		AgentMessage: &AgentMessage{From: "d1:a", To: "d1:b"},
		Escalated:    &EscalationNotice{Conflicts: []string{"c1"}, Trigger: TriggerAgent, Reason: "who owns the schema", Urgent: true},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"stances":{"c1":["claim",""]}`, `"message":{"from":"d1:a","to":"d1:b"}`,
		`"escalated":{"conflicts":["c1"],"trigger":"agent","reason":"who owns the schema","urgent":true}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s lacks %s", b, want)
		}
	}
	var out Message
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.AgentMessage == nil || *out.AgentMessage != *in.AgentMessage || out.Stances["c1"] != in.Stances["c1"] ||
		out.Escalated == nil || out.Escalated.Reason != in.Escalated.Reason {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

// TestOfficeViewEmptySlices: an office repo with nothing in it marshals every
// list as [], never null, so a client iterates without checks.
func TestOfficeViewEmptySlices(t *testing.T) {
	b, err := json.Marshal(OfficeView{Repos: []OfficeRepo{NewOfficeRepo("github.com/x/y")}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "null") {
		t.Errorf("%s has a null", b)
	}
	for _, want := range []string{`"workstreams":[]`, `"shared":[]`, `"conflicts":[]`, `"messages":[]`, `"escalations":[]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s lacks %s", b, want)
		}
	}
}
