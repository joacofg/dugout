package core

import "testing"

// escConflict is a conflict between d1:a and d2:b with the stances given, in
// side order.
func escConflict(s0, s1 Stance) Conflict {
	return Conflict{ID: "c1", Repo: "r", Path: "x.go", Kind: "content",
		Sides:   [2]Side{{Daemon: "d1", Workstream: "d1:a"}, {Daemon: "d2", Workstream: "d2:b"}},
		Stances: [2]Stance{s0, s1}}
}

// TestEscalationTimeRule walks an injected clock past the rule's delay: the
// conflict goes to the humans only once both sides were warned and the later
// warning is After old.
func TestEscalationTimeRule(t *testing.T) {
	const minute = Millis(60_000)
	rule := EscalationRule{After: 10 * minute}
	c := escConflict(StanceNone, StanceNone)
	now := Millis(1_000_000)
	clock := func(step Millis) Millis { now += step; return now }

	warned := [2]Millis{now, 0}
	if got := rule.Decide(c, warned, clock(60*minute)); got != TriggerNone {
		t.Fatalf("one side warned an hour ago: %q, want none", got)
	}
	warned[1] = clock(0)
	if got := rule.Decide(c, warned, clock(10*minute-1)); got != TriggerNone {
		t.Fatalf("a millisecond before the delay since the later warning: %q, want none", got)
	}
	if got := rule.Decide(c, warned, clock(1)); got != TriggerTime {
		t.Fatalf("at the delay: %q, want %q", got, TriggerTime)
	}
	if got := rule.Decide(c, warned, clock(24*60*minute)); got != TriggerTime {
		t.Fatalf("a day later: %q, want %q", got, TriggerTime)
	}
	if at, ok := rule.DueAt(warned); !ok || at != warned[1]+10*minute {
		t.Fatalf("DueAt = %d, %v; want %d, true", at, ok, warned[1]+10*minute)
	}
	if _, ok := rule.DueAt([2]Millis{warned[0], 0}); ok {
		t.Fatal("DueAt with one side not warned is due")
	}
}

// TestEscalationStances: an agreed yield never escalates; both branches
// claiming the span is agents that cannot agree, and escalates at once, even
// with the time rule off. The other stances leave the time rule to decide.
func TestEscalationStances(t *testing.T) {
	const minute = Millis(60_000)
	on, off := EscalationRule{After: 10 * minute}, EscalationRule{}
	warned := [2]Millis{1000, 2000}
	early, late := Millis(3000), 2000+10*minute
	cases := []struct {
		name        string
		s0, s1      Stance
		early, late Trigger
		off         Trigger // late, time rule off
	}{
		{"nobody said anything", StanceNone, StanceNone, TriggerNone, TriggerTime, TriggerNone},
		{"one side yielded", StanceYield, StanceNone, TriggerNone, TriggerNone, TriggerNone},
		{"the other side yielded", StanceNone, StanceYield, TriggerNone, TriggerNone, TriggerNone},
		{"one claimed, the other yielded", StanceClaim, StanceYield, TriggerNone, TriggerNone, TriggerNone},
		{"one side claimed alone", StanceClaim, StanceNone, TriggerNone, TriggerTime, TriggerNone},
		{"both yielded: messages crossed", StanceYield, StanceYield, TriggerNone, TriggerTime, TriggerNone},
		{"both claimed", StanceClaim, StanceClaim, TriggerDisputed, TriggerDisputed, TriggerDisputed},
	}
	for _, tc := range cases {
		c := escConflict(tc.s0, tc.s1)
		if got := on.Decide(c, warned, early); got != tc.early {
			t.Errorf("%s, before the delay: %q, want %q", tc.name, got, tc.early)
		}
		if got := on.Decide(c, warned, late); got != tc.late {
			t.Errorf("%s, after the delay: %q, want %q", tc.name, got, tc.late)
		}
		if got := off.Decide(c, warned, late); got != tc.off {
			t.Errorf("%s, time rule off: %q, want %q", tc.name, got, tc.off)
		}
	}
	if !escConflict(StanceYield, StanceClaim).Agreed() || escConflict(StanceYield, StanceYield).Agreed() || escConflict(StanceClaim, StanceNone).Agreed() {
		t.Fatal("Agreed: only a yield the other side did not also make settles the span")
	}
	if !escConflict(StanceClaim, StanceClaim).Disputed() || escConflict(StanceClaim, StanceYield).Disputed() {
		t.Fatal("Disputed: only both claims")
	}
}

// TestEscalationRuleOff: a zero or negative delay never fires the time rule.
func TestEscalationRuleOff(t *testing.T) {
	c := escConflict(StanceNone, StanceNone)
	for _, after := range []Millis{0, -1} {
		r := EscalationRule{After: after}
		if got := r.Decide(c, [2]Millis{1, 1}, 1<<50); got != TriggerNone {
			t.Errorf("After %d: %q, want none", after, got)
		}
		if _, ok := r.DueAt([2]Millis{1, 1}); ok {
			t.Errorf("After %d: DueAt is due", after)
		}
	}
}
