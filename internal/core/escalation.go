package core

// Millis is a moment in milliseconds on a clock the caller chooses, or a span
// of them: core reads no clock, so the time rule is handed the time.
type Millis int64

// Trigger is why a conflict goes to both humans; TriggerNone, that it does
// not.
type Trigger string

const (
	TriggerNone Trigger = ""
	// TriggerTime: the conflict still stands After the later of the warnings
	// of its two sides.
	TriggerTime Trigger = "time"
	// TriggerDisputed: both sides claimed the span, so the agents could not
	// agree on the channel.
	TriggerDisputed Trigger = "disputed"
)

// EscalationRule is Dugout's fixed rule for what goes to the humans without
// an agent asking: a conflict still standing After both sides
// were warned, or one both sides claim. After at or below zero turns the time
// rule off; a disputed span escalates all the same.
type EscalationRule struct {
	After Millis
}

// Agreed reports whether c's span was settled on the channel: a side yielded
// it and the other did not yield it too. An agreed yield is a dependency
// between the two branches, not a question for the humans, so it never
// escalates, even while the lines still conflict.
func (c Conflict) Agreed() bool {
	return (c.Stances[0] == StanceYield) != (c.Stances[1] == StanceYield)
}

// Disputed reports whether both sides of c claimed its span.
func (c Conflict) Disputed() bool {
	return c.Stances[0] == StanceClaim && c.Stances[1] == StanceClaim
}

// DueAt is when the time rule fires for a conflict whose sides were warned
// at warned (0: not yet), and whether it ever does: only once both were, and
// only with the rule on.
func (r EscalationRule) DueAt(warned [2]Millis) (Millis, bool) {
	if r.After <= 0 || warned[0] == 0 || warned[1] == 0 {
		return 0, false
	}
	return max(warned[0], warned[1]) + r.After, true
}

// Decide is why c, standing at now with its sides warned at warned (0: not
// yet), goes to both humans, or TriggerNone. A dispute comes first: it needs
// no waiting. An agreed yield never escalates.
func (r EscalationRule) Decide(c Conflict, warned [2]Millis, now Millis) Trigger {
	switch {
	case c.Disputed():
		return TriggerDisputed
	case c.Agreed():
		return TriggerNone
	}
	if at, ok := r.DueAt(warned); ok && now >= at {
		return TriggerTime
	}
	return TriggerNone
}
