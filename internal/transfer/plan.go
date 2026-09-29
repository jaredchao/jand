package transfer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A plan is the goals after the first that the host writes down when it
// starts a chat. It travels in the sealed charter, and the guest's user
// agrees to it, word for word, by joining. At a checkpoint each side may then
// let jand move on to the plan's next goal without asking again, as its own
// user chose: ask (the default) asks every time, auto moves on at once, and
// a duration waits that long for the user first. Only the plan's next goal,
// compared exactly, can be taken this way; anything else waits for the user.
// See docs/CORE.md.

// PlanStep is one planned goal. Budget 0 leaves it to the relay's default.
type PlanStep struct {
	Goal   string `json:"goal"`
	Budget int    `json:"budget,omitempty"`
}

const MaxPlanSteps = 10

func checkPlan(p []PlanStep) error {
	if len(p) > MaxPlanSteps {
		return fmt.Errorf("a plan has at most %d goals after the first", MaxPlanSteps)
	}
	for i, step := range p {
		if err := checkGoal(step.Goal, step.Budget); err != nil {
			return fmt.Errorf("plan goal %d: %w", i+1, err)
		}
	}
	return nil
}

// ParsePlan reads a plan file: a JSON array of {goal, budget}, or a Markdown
// list with one goal per item ("- ..." or "1. ..."). An indented line
// continues the item above it; other lines are ignored. Markdown items take
// the default budget.
func ParsePlan(data []byte) ([]PlanStep, error) {
	var steps []PlanStep
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '[' {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&steps); err != nil {
			return nil, fmt.Errorf("plan: %w", err)
		}
	} else {
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			line := sc.Text()
			item, ok := listItem(line)
			switch {
			case ok:
				steps = append(steps, PlanStep{Goal: item})
			case len(steps) > 0 && strings.TrimSpace(line) != "" && (line[0] == ' ' || line[0] == '\t'):
				steps[len(steps)-1].Goal += " " + strings.TrimSpace(line)
			}
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("plan: %w", err)
		}
	}
	if len(steps) == 0 {
		return nil, errors.New("plan: no goals found; write a Markdown list or a JSON array of {goal, budget}")
	}
	return steps, checkPlan(steps)
}

// listItem recognises an unindented Markdown list item.
func listItem(line string) (string, bool) {
	for _, bullet := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(line, bullet) {
			return strings.TrimSpace(line[len(bullet):]), true
		}
	}
	if i := strings.IndexAny(line, ".)"); i > 0 && i < len(line)-1 && line[i+1] == ' ' {
		if _, err := strconv.Atoi(line[:i]); err == nil {
			return strings.TrimSpace(line[i+2:]), true
		}
	}
	return "", false
}

// ParseAdvance checks an advance mode: ask, auto, or a duration from one
// minute to six hours to wait for the user first. It returns the canonical form.
func ParseAdvance(s string) (string, error) {
	switch s {
	case "", "ask":
		return "ask", nil
	case "auto":
		return "auto", nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Minute || d > 6*time.Hour {
		return "", fmt.Errorf("advance must be ask, auto, or a duration from 1m to 6h (e.g. 5m); got %q", s)
	}
	// 5m rather than Go's 5m0s: people read it on the page.
	s = d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s, nil
}

// advanceDelay is how long after a checkpoint this side may move on to the
// plan's next goal by itself; ok is false when it must always ask.
func (s *chatState) advanceDelay() (time.Duration, bool) {
	switch s.Advance {
	case "", "ask":
		return 0, false
	case "auto":
		return 0, true
	}
	d, err := time.ParseDuration(s.Advance)
	return d, err == nil
}

// planState is where this side is in the plan. recv and the foreground
// commands both change it, so it has a file of its own and every change is
// read, applied and written at once.
type planState struct {
	// Next indexes the plan goal that comes next; goals taken outside the
	// plan do not move it.
	Next int `json:"next"`
	// PausedAt is when this side saw the current checkpoint (Unix seconds);
	// 0 while the chat runs.
	PausedAt int64 `json:"paused_at,omitempty"`
	// Proposed: this side proposed a goal at this checkpoint, so it is not
	// jand's to propose one.
	Proposed bool `json:"proposed,omitempty"`
	// Due: advance_due was already given for this checkpoint.
	Due bool `json:"due,omitempty"`
	// Off: the user turned automatic advance off for this chat.
	Off bool `json:"off,omitempty"`
}

func (s *chatState) loadPlan(dir string) planState {
	var p planState
	readJSONFile(s.path(dir, ".plan"), &p)
	return p
}

func (s *chatState) updatePlan(dir string, change func(*planState)) planState {
	p := s.loadPlan(dir)
	change(&p)
	writeJSONFile(s.path(dir, ".plan"), p)
	return p
}

// paused records a checkpoint: the time the users' decision starts.
func (s *chatState) paused(dir string) planState {
	if len(s.Plan) == 0 {
		return planState{}
	}
	return s.updatePlan(dir, func(p *planState) {
		p.PausedAt, p.Proposed, p.Due = time.Now().Unix(), false, false
	})
}

// advanceAt is when this side may take the plan's next goal at the current
// checkpoint, for the checkpoint event: zero when it may not.
func (s *chatState) advanceAt(p planState) time.Time {
	d, ok := s.advanceDelay()
	if !ok || p.Off || p.PausedAt == 0 || p.Next >= len(s.Plan) {
		return time.Time{}
	}
	return time.Unix(p.PausedAt, 0).Add(d)
}

// planned reports whether a proposal is word for word the plan's next goal.
func (s *chatState) planned(p planState, goal string, budget int) bool {
	if p.Next >= len(s.Plan) {
		return false
	}
	step := s.Plan[p.Next]
	return step.Goal == goal && (step.Budget == 0 || step.Budget == budget)
}

// pendingAdvance is when advance_due should be given, if it should: the
// host may propose the next goal once the wait is over; the guest may accept
// once that goal has also been proposed.
func (s *chatState) pendingAdvance(p planState, cur chatCursor) (time.Time, bool) {
	at := s.advanceAt(p)
	if at.IsZero() || p.Due || p.Proposed {
		return at, false
	}
	if s.Role == "guest" && (cur.Proposal == 0 || !s.planned(p, cur.ProposalGoal, cur.ProposalBudget)) {
		return at, false
	}
	return at, true
}

// takeDue returns the advance_due event once its time has come, and marks
// it given so recv does not repeat it at this checkpoint.
func (s *chatState) takeDue(dir string, cur chatCursor) (Event, bool) {
	p := s.loadPlan(dir)
	at, ok := s.pendingAdvance(p, cur)
	if !ok || time.Now().Before(at) {
		return Event{}, false
	}
	s.updatePlan(dir, func(p *planState) { p.Due = true })
	step := s.Plan[p.Next]
	return Event{Event: "advance_due", Chat: s.ID, PlanStep: p.Next + 2, Goal: step.Goal, Budget: step.Budget,
		Advance: s.Advance, Message: AdvanceNote}, true
}

// AdvanceNote goes with advance_due. The wait was for the user; if they
// answered meanwhile, their answer stands.
const AdvanceNote = "The wait your user allowed is over. If they answered meanwhile, do what they said " +
	"(and run chat advance --off if they want to decide each step). Otherwise run chat advance to take the plan's next goal, and tell them."

// NextPlanStep returns the plan goal that comes next in a chat, for a user
// who decides to propose it: proposing it word for word lets the peer's
// jand recognise it.
func NextPlanStep(id string, opts Options) (PlanStep, int, error) {
	o := opts.defaults()
	s, err := loadChat(o.StateDir, id)
	if err != nil {
		return PlanStep{}, 0, err
	}
	p := s.loadPlan(o.StateDir)
	if p.Next >= len(s.Plan) {
		return PlanStep{}, 0, errors.New("this chat has no planned goal left")
	}
	return s.Plan[p.Next], p.Next + 2, nil
}

// ChatAdvance takes the plan's next goal at a checkpoint without asking the
// user again, as they allowed: the host proposes it, the guest accepts it
// once the host has proposed exactly that. With off it instead turns
// automatic advance off for the rest of this chat.
func ChatAdvance(ctx context.Context, id string, off bool, opts Options) error {
	o := opts.defaults()
	s, err := loadChat(o.StateDir, id)
	if err != nil {
		return err
	}
	if off {
		s.updatePlan(o.StateDir, func(p *planState) { p.Off = true })
		s.writeRecord(o.StateDir, TranscriptLine{From: s.Role, Kind: "advance_off"})
		o.Emit(Event{Event: "advance_off", Chat: s.ID})
		return nil
	}
	if len(s.Plan) == 0 {
		return errors.New("this chat has no plan; ask your user")
	}
	d, ok := s.advanceDelay()
	if !ok {
		return errors.New("your user chose to decide each goal in this chat (advance: ask); ask them")
	}
	p := s.loadPlan(o.StateDir)
	switch {
	case p.Off:
		return errors.New("automatic advance was turned off for this chat; ask your user")
	case p.PausedAt == 0:
		return errors.New("the chat is not at a checkpoint")
	case p.Next >= len(s.Plan):
		return errors.New("the plan is finished; ask your user whether to end or set a new goal")
	case p.Proposed:
		return errors.New("a goal was already proposed at this checkpoint")
	}
	if at := time.Unix(p.PausedAt, 0).Add(d); time.Now().Before(at) {
		return fmt.Errorf("not yet: your user has until %s to decide", at.Local().Format("15:04:05"))
	}
	step := s.Plan[p.Next]
	if s.Role == "host" {
		if err = ChatPropose(ctx, s.ID, step.Goal, step.Budget, o); err != nil {
			return err
		}
	} else {
		var cur chatCursor
		readJSONFile(s.path(o.StateDir, ".cursor"), &cur)
		if cur.Proposal == 0 || !s.planned(p, cur.ProposalGoal, cur.ProposalBudget) {
			return fmt.Errorf("the peer has not proposed plan goal %d; only that goal can be accepted without asking", p.Next+2)
		}
		if err = ChatAccept(ctx, s.ID, o); err != nil {
			return err
		}
	}
	s.writeRecord(o.StateDir, TranscriptLine{From: s.Role, Kind: "advanced", Text: step.Goal, Budget: step.Budget, Step: p.Next + 2, Mode: s.Advance})
	o.Emit(Event{Event: "advanced", Chat: s.ID, PlanStep: p.Next + 2, Goal: step.Goal, Budget: step.Budget, Advance: s.Advance})
	return nil
}

// ended stops any advance once the chat is over.
func (s *chatState) ended(dir string) {
	if len(s.Plan) > 0 {
		s.updatePlan(dir, func(p *planState) { p.PausedAt = 0 })
	}
}

// stamp formats an event time; empty for the zero time.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
