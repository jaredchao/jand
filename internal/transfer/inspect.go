package transfer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// RecordIntegrity describes this machine's recording coverage, not relay truth.
// A partial trailing write is excluded. Broken records never yield usable totals.
type RecordIntegrity struct {
	State       string `json:"state"` // complete, partial, missing, empty, broken
	Reason      string `json:"reason,omitempty"`
	ValidLines  int    `json:"valid_lines"`
	ProblemLine int    `json:"problem_line,omitempty"`
	StartedLate bool   `json:"started_late,omitempty"`
}

type InspectTotals struct {
	Wakes      int   `json:"wakes"`
	EmptyWakes int   `json:"empty_wakes"`
	Decisions  int   `json:"decisions"`
	OutBytes   int64 `json:"out_bytes"`
	WroteBytes int64 `json:"wrote_bytes"`
}

type InspectCost struct {
	Integrity   RecordIntegrity `json:"integrity"`
	Records     []CostRecord    `json:"records"`
	Totals      *InspectTotals  `json:"totals"`       // null for missing, empty or broken
	WallSeconds *float64        `json:"wall_seconds"` // local observed span; not CPU time
}

type InspectMessage struct {
	ID           string   `json:"id"`
	Section      int      `json:"section"`
	From         string   `json:"from"`
	Kind         string   `json:"kind"`
	Text         string   `json:"text"`
	ReplyTo      string   `json:"reply_to,omitempty"`
	Answers      []string `json:"answers"`
	Pending      bool     `json:"pending"`
	Supersedes   string   `json:"supersedes,omitempty"`
	SupersededBy string   `json:"superseded_by,omitempty"`
}

type InspectSection struct {
	N               int            `json:"n"`
	Goal            string         `json:"goal"`
	Budget          int            `json:"budget"`
	Started         string         `json:"started"`
	Messages        int            `json:"messages"`
	Outcome         string         `json:"outcome,omitempty"`
	TimelineIndexes []int          `json:"timeline_indexes"`
	SummaryIndexes  []int          `json:"summary_indexes"`
	Deliveries      []string       `json:"deliveries"`
	Cost            *InspectTotals `json:"cost"`
	CostState       string         `json:"cost_state"`
	WallSeconds     *float64       `json:"wall_seconds"`
}

type InspectPlan struct {
	Steps      []PlanStep      `json:"steps"`
	Advance    string          `json:"advance"`
	Proposal   *TranscriptLine `json:"proposal"`
	Checkpoint *TranscriptLine `json:"checkpoint"`
}

// ChatInspection is a whitelist projection; it never serializes chatState,
// capabilities, keys, cursors or config. User-authored text is untrusted content.
type ChatInspection struct {
	SchemaVersion       int              `json:"schema_version"`
	Scope               string           `json:"scope"`
	Chat                ChatSummary      `json:"chat"`
	TranscriptIntegrity RecordIntegrity  `json:"transcript_integrity"`
	Timeline            []TranscriptLine `json:"timeline"`
	Sections            []InspectSection `json:"sections"`
	Messages            []InspectMessage `json:"messages"`
	Plan                InspectPlan      `json:"plan"`
	Cost                InspectCost      `json:"cost"`
}

// readInspectLines keeps a valid prefix but never hides damage. Error reasons
// contain no input text or paths, so a corrupt credential cannot leak in errors.
func readInspectLines[T any](path string, valid func(T, []byte) bool) ([]T, RecordIntegrity) {
	rows := []T{}
	meta := RecordIntegrity{State: "complete"}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		meta.State = "missing"
		return rows, meta
	}
	if err != nil {
		meta.State = "broken"
		meta.Reason = "unreadable"
		return rows, meta
	}
	defer f.Close()
	const limit = 64 << 20
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(data) > limit {
		meta.State = "broken"
		meta.Reason = "read_failed_or_too_large"
		return rows, meta
	}
	if len(data) == 0 {
		meta.State = "empty"
		return rows, meta
	}
	parts := bytes.Split(data, []byte{'\n'})
	for i, b := range parts {
		if i == len(parts)-1 {
			if len(b) > 0 {
				meta.State = "partial"
				meta.Reason = "unterminated_tail"
				meta.ProblemLine = i + 1
			}
			break
		}
		var row T
		if json.Unmarshal(b, &row) != nil || !valid(row, b) {
			meta.State = "broken"
			meta.Reason = "invalid_record"
			meta.ProblemLine = i + 1
			break
		}
		rows = append(rows, row)
	}
	meta.ValidLines = len(rows)
	return rows, meta
}

func validTranscript(l TranscriptLine, _ []byte) bool {
	_, err := time.Parse(time.RFC3339, l.Time)
	return err == nil && l.Kind != "" && slices.Contains([]string{"host", "guest", "relay"}, l.From) && l.Budget >= 0 && (l.Kind != "message" || l.ID != "" && l.Type != "")
}

func validCost(r CostRecord, b []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return false
	}
	for _, key := range []string{"out", "wrote", "exit", "empty", "check", "own"} {
		if bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return false
		}
	}
	_, err := time.Parse(time.RFC3339, r.Time)
	return err == nil && r.Cmd != "" && r.Out >= 0 && r.Wrote >= 0 && r.Exit >= 0 && fields["out"] != nil && fields["exit"] != nil
}

func addInspectCost(t *InspectTotals, r CostRecord) {
	if r.Wake() {
		t.Wakes++
		if r.Empty {
			t.EmptyWakes++
		}
	}
	if r.Exit == 0 && slices.Contains([]string{"join", "propose", "accept", "close", "checkpoint"}, r.Cmd) {
		t.Decisions++
	}
	t.OutBytes += r.Out
	t.WroteBytes += r.Wrote
}

// InspectChat reads local files only. Unlike recv/watch it never contacts a
// relay or loads/moves a cursor. Unlike log/view it never records its own cost.
func InspectChat(dir, id string) (ChatInspection, error) {
	s, err := loadChat(dir, id)
	if err != nil {
		return ChatInspection{}, fmt.Errorf("cannot inspect local chat: %w", err)
	}
	// A state ID must match the requested filename/prefix before deriving paths.
	if len(s.ID) != 32 || bytes.IndexFunc([]byte(s.ID), func(r rune) bool { return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) }) >= 0 {
		return ChatInspection{}, errors.New("invalid local chat id")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, strings.ToLower(strings.TrimSpace(id))+"*.json"))
	for _, path := range matches {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		if len(name) == 32 && name != s.ID {
			return ChatInspection{}, errors.New("local chat id does not match its state filename")
		}
	}
	lines, integrity := readInspectLines(s.path(dir, ".transcript.jsonl"), validTranscript)
	if (integrity.State == "complete" || integrity.State == "partial") && len(lines) > 0 && lines[0].Kind != "started" && lines[0].Kind != "joined" {
		integrity.StartedLate = true
		if integrity.State == "complete" {
			integrity.State, integrity.Reason = "partial", "late_recording"
		}
	}
	c := summarize(s, lines)
	// Only an origin is public metadata. Never emit credentials, query or fragment.
	c.Relay = ""
	if u, e := url.Parse(s.Relay); e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		c.Relay = u.Scheme + "://" + u.Host
	}
	if integrity.State != "complete" {
		c.Status = "unknown"
	}
	d := ChatInspection{SchemaVersion: 1, Scope: "local", Chat: c, TranscriptIntegrity: integrity, Timeline: lines, Sections: []InspectSection{}, Messages: []InspectMessage{}, Plan: InspectPlan{Steps: append([]PlanStep{}, s.Plan...), Advance: s.Advance}}
	if d.Plan.Advance == "" {
		d.Plan.Advance = "ask"
	}
	byID := map[string]int{}
	for i, l := range lines {
		if len(d.Sections) == 0 || l.Kind == "resumed" {
			goal := ""
			if l.Kind == "started" || l.Kind == "joined" || l.Kind == "resumed" {
				goal = l.Text
			}
			d.Sections = append(d.Sections, InspectSection{N: len(d.Sections) + 1, Goal: goal, Budget: l.Budget, Started: l.Time, TimelineIndexes: []int{}, SummaryIndexes: []int{}, Deliveries: []string{}})
		}
		sec := &d.Sections[len(d.Sections)-1]
		sec.TimelineIndexes = append(sec.TimelineIndexes, i)
		switch l.Kind {
		case "message":
			sec.Messages++
			byID[l.ID] = len(d.Messages)
			d.Messages = append(d.Messages, InspectMessage{ID: l.ID, Section: sec.N, From: l.From, Kind: l.Type, Text: l.Text, ReplyTo: l.ReplyTo, Supersedes: l.Supersedes, Answers: []string{}})
		case "done":
			sec.SummaryIndexes = append(sec.SummaryIndexes, i)
		case "checkpoint":
			sec.Outcome = l.Kind
			copy := l
			d.Plan.Checkpoint = &copy
		case "closed", "expired", "decline":
			sec.Outcome = l.Kind
			d.Plan.Proposal = nil
		case "proposal":
			copy := l
			d.Plan.Proposal = &copy
		case "resumed":
			d.Plan.Proposal = nil
			d.Plan.Checkpoint = nil
		}
	}
	for _, m := range d.Messages {
		if j, ok := byID[m.ReplyTo]; ok {
			d.Messages[j].Answers = append(d.Messages[j].Answers, m.ID)
		}
		if j, ok := byID[m.Supersedes]; ok {
			d.Messages[j].SupersededBy = m.ID
		}
	}
	for i := range d.Messages {
		m := &d.Messages[i]
		m.Pending = m.Kind == "request" && len(m.Answers) == 0
		if m.Kind == "delivery" && m.SupersededBy == "" {
			sec := &d.Sections[m.Section-1]
			sec.Deliveries = append(sec.Deliveries, m.ID)
		}
	}
	recs, ci := readInspectLines(s.path(dir, ".cost.jsonl"), validCost)
	var totalOut, totalWrote int64
	for _, r := range recs {
		if r.Out > math.MaxInt64-totalOut || r.Wrote > math.MaxInt64-totalWrote {
			ci.State, ci.Reason = "broken", "numeric_overflow"
			break
		}
		totalOut += r.Out
		totalWrote += r.Wrote
	}
	if (ci.State == "complete" || ci.State == "partial") && len(recs) > 0 && recs[0].Cmd != "start" && recs[0].Cmd != "claim" {
		ci.StartedLate = true
		if ci.State == "complete" {
			ci.State, ci.Reason = "partial", "late_recording"
		}
	}
	d.Cost = InspectCost{Integrity: ci, Records: recs}
	for i := range d.Sections {
		d.Sections[i].CostState = ci.State
		if integrity.State != "complete" {
			d.Sections[i].CostState = "unavailable"
		}
	}
	if (ci.State == "complete" || ci.State == "partial") && len(recs) > 0 {
		d.Cost.Totals = &InspectTotals{}
		for i := range d.Sections {
			d.Sections[i].Cost = &InspectTotals{}
		}
		for _, r := range recs {
			addInspectCost(d.Cost.Totals, r)
			// Section attribution is available only for a complete transcript.
			if integrity.State != "complete" {
				continue
			}
			t, _ := time.Parse(time.RFC3339, r.Time)
			n := -1
			for j, sec := range d.Sections {
				start, _ := time.Parse(time.RFC3339, sec.Started)
				if !t.Before(start) {
					n = j
				}
			}
			if n >= 0 {
				addInspectCost(d.Sections[n].Cost, r)
			}
		}
		if integrity.State != "complete" {
			for i := range d.Sections {
				d.Sections[i].Cost = nil
			}
		}
		if integrity.State == "complete" && ci.StartedLate {
			first, _ := time.Parse(time.RFC3339, recs[0].Time)
			for i := 0; i+1 < len(d.Sections); i++ {
				end, _ := time.Parse(time.RFC3339, d.Sections[i+1].Started)
				if !first.Before(end) {
					d.Sections[i].Cost = nil
					d.Sections[i].CostState = "missing"
				}
			}
		}
	}
	if integrity.State == "complete" && len(d.Sections) > 0 {
		last := c.Last
		if ci.State == "complete" || ci.State == "partial" {
			for _, r := range recs {
				t, _ := time.Parse(time.RFC3339, r.Time)
				if t.After(last) {
					last = t
				}
			}
		}
		start, _ := time.Parse(time.RFC3339, d.Sections[0].Started)
		span := last.Sub(start).Seconds()
		if span >= 0 {
			d.Cost.WallSeconds = &span
		}
		for i := range d.Sections {
			end := last
			if i+1 < len(d.Sections) {
				end, _ = time.Parse(time.RFC3339, d.Sections[i+1].Started)
			}
			begin, _ := time.Parse(time.RFC3339, d.Sections[i].Started)
			seconds := end.Sub(begin).Seconds()
			if seconds >= 0 {
				d.Sections[i].WallSeconds = &seconds
			}
		}
	}
	return d, nil
}
