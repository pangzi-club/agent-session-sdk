package agentsession

import "math"

// TallyEventKind mirrors TallyEventKind in the usage TS layer.
type TallyEventKind int

const (
	TallyPrompt TallyEventKind = iota
	TallyAssistant
	TallyTool
)

type tallyEvent struct {
	ts    float64
	kind  TallyEventKind
	tool  *string
	skill *string
	cum   float64
}

// TallyInterval mirrors TallyInterval: fields map one to one onto UsageRecord's
// per-conversation fields.
type TallyInterval struct {
	UserPrompts      float64
	ToolCalls        float64
	ToolCallsByName  map[string]float64
	SkillCallsByName map[string]float64
	ActiveMs         float64
}

// SessionTally mirrors SessionTally in the usage TS layer: feed events in
// ascending time order, take increments with TakeUntil at record points, and
// Flush the remainder into the session's last record. Out-of-order events are
// dropped and a single gap counts at most ActiveGapCapMs.
type SessionTally struct {
	events []tallyEvent
	cursor int
}

// AddEvent feeds one activity event; skill is an optional argument holding the
// skill name carried by a Skill tool call (only the claude, qoder and zcode
// usage extractors pass it; the other scanners omit it).
func (t *SessionTally) AddEvent(ts float64, kind TallyEventKind, tool *string, skill ...*string) {
	if math.IsNaN(ts) || math.IsInf(ts, 0) || ts <= 0 {
		return
	}
	var last *tallyEvent
	if len(t.events) > 0 {
		last = &t.events[len(t.events)-1]
	}
	if last != nil && ts < last.ts {
		return
	}
	gap := 0.0
	if last != nil && ts > last.ts {
		gap = math.Min(ts-last.ts, ActiveGapCapMs)
	}
	base := 0.0
	if last != nil {
		base = last.cum
	}
	var skillName *string
	if len(skill) > 0 {
		skillName = skill[0]
	}
	t.events = append(t.events, tallyEvent{ts: ts, kind: kind, tool: tool, skill: skillName, cum: base + gap})
}

// TakeUntil drains the increment of events with timestamp <= ts.
func (t *SessionTally) TakeUntil(ts float64) TallyInterval {
	return t.take(func(e tallyEvent) bool { return e.ts <= ts })
}

// Flush drains the remaining increment (session close-out).
func (t *SessionTally) Flush() TallyInterval {
	return t.take(func(tallyEvent) bool { return true })
}

func (t *SessionTally) take(keep func(tallyEvent) bool) TallyInterval {
	base := 0.0
	if t.cursor > 0 {
		base = t.events[t.cursor-1].cum
	}
	interval := TallyInterval{}
	for t.cursor < len(t.events) && keep(t.events[t.cursor]) {
		event := t.events[t.cursor]
		switch event.kind {
		case TallyPrompt:
			interval.UserPrompts++
		case TallyTool:
			interval.ToolCalls++
			if event.tool != nil {
				if interval.ToolCallsByName == nil {
					interval.ToolCallsByName = map[string]float64{}
				}
				interval.ToolCallsByName[*event.tool]++
			}
			if event.skill != nil {
				if interval.SkillCallsByName == nil {
					interval.SkillCallsByName = map[string]float64{}
				}
				interval.SkillCallsByName[*event.skill]++
			}
		}
		t.cursor++
	}
	cum := 0.0
	if t.cursor > 0 {
		cum = t.events[t.cursor-1].cum
	}
	interval.ActiveMs = math.Max(0, cum-base)
	return interval
}
