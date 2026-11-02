// Package taskcontrol owns the local, host-independent collaboration ledger.
// Nothing here sends messages, launches agents, or trusts peer text as authority.
package taskcontrol

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Scope struct {
	Host  string `json:"host"`
	Side  string `json:"side"`
	Chat  string `json:"chat"`
	Phase string `json:"phase"`
}
type Limits struct {
	Running int `json:"running"`
	Queued  int `json:"queued"`
}
type Task struct {
	ID       string `json:"id"`
	Version  int    `json:"version"`
	Text     string `json:"text"`
	State    string `json:"state"`
	Evidence string `json:"evidence,omitempty"`
}
type Run struct {
	ID       string `json:"id"`
	Task     string `json:"task"`
	Version  int    `json:"version"`
	Session  string `json:"session"`
	State    string `json:"state"`
	Evidence string `json:"evidence,omitempty"`
}
type Coordinator struct {
	Kind    string `json:"kind,omitempty"`
	Session string `json:"session"`
	State   string `json:"state"`
}
type Notice struct {
	Revision int    `json:"revision"`
	State    string `json:"state"`
	Version  int    `json:"version"`
}
type Receipt struct {
	Request     Request `json:"request"`
	Fingerprint string  `json:"fingerprint"`
	Outcome     string  `json:"outcome"`
	Task        string  `json:"task,omitempty"`
}
type State struct {
	headroom    capacity
	Schema      int                `json:"schema"`
	Scope       Scope              `json:"scope"`
	Owner       string             `json:"owner"`
	Limits      Limits             `json:"limits"`
	ControlID   int                `json:"control_id"`
	Halted      bool               `json:"halted"`
	Mode        string             `json:"mode"`
	Coordinator *Coordinator       `json:"coordinator,omitempty"`
	Tasks       map[string]*Task   `json:"tasks"`
	Order       []string           `json:"order"`
	Runs        map[string]*Run    `json:"runs"`
	Events      map[string]Receipt `json:"events"`
	Notices     map[string]Notice  `json:"notices"`
	Revision    int                `json:"revision"`
}
type Request struct {
	Op       string  `json:"op"`
	Scope    Scope   `json:"scope"`
	Owner    string  `json:"owner"`
	Event    string  `json:"event,omitempty"`
	Task     string  `json:"task,omitempty"`
	Version  int     `json:"version,omitempty"`
	Text     string  `json:"text,omitempty"`
	Run      string  `json:"run,omitempty"`
	Status   string  `json:"status,omitempty"`
	Evidence string  `json:"evidence,omitempty"`
	Limits   *Limits `json:"limits,omitempty"`
	Mode     string  `json:"mode,omitempty"`
	Revision int     `json:"revision,omitempty"`
}
type Response struct {
	Replayed    bool         `json:"replayed,omitempty"`
	Outcome     string       `json:"outcome"`
	Task        *Task        `json:"task,omitempty"`
	Run         *Run         `json:"run,omitempty"`
	State       *State       `json:"state,omitempty"`
	Coordinator *Coordinator `json:"coordinator,omitempty"`
}

func id() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func Key(s Scope) string {
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func active(s string) bool {
	return s == "starting" || s == "running" || s == "canceling" || s == "unknown"
}
func (s *State) notice(t *Task) {
	s.Revision++
	s.Notices[t.ID] = Notice{s.Revision, t.State, t.Version}
}
func (s *State) counts() (running, queued int) {
	for _, r := range s.Runs {
		if active(r.State) || r.State == "result_pending" {
			running++
		}
	}
	for _, t := range s.Tasks {
		if t.State == "blocked" && t.Evidence == "paused" {
			running++
		}
		if t.State == "queued" || t.State == "blocked" && t.Evidence == "awaiting_prior_stop" {
			queued++
		}
	}
	return
}
func (s *State) outstanding(t string) bool {
	for _, r := range s.Runs {
		if r.Task == t && active(r.State) {
			return true
		}
	}
	return false
}
func validScope(s Scope) bool {
	return strings.TrimSpace(s.Host) != "" && strings.TrimSpace(s.Side) != "" && strings.TrimSpace(s.Chat) != "" && strings.TrimSpace(s.Phase) != ""
}
func (s *State) validate() error {
	if s.Schema != 1 || !validScope(s.Scope) || s.Owner == "" || s.Limits.Running < 1 || s.Limits.Running > 16 || s.Limits.Queued < 1 || s.Limits.Queued > 1024 || s.Tasks == nil || s.Runs == nil || s.Events == nil || s.Notices == nil {
		return errors.New("invalid task ledger; recovery required")
	}
	if s.Mode != "three_level" && s.Mode != "two_level" && s.Mode != "manual" {
		return errors.New("invalid mode")
	}
	if s.Coordinator != nil {
		c := s.Coordinator
		if c.Session == "" || !strings.Contains("|starting|running|idle|canceling|stopped|unknown|", "|"+c.State+"|") || c.Kind != "" && c.Kind != "independent" && c.Kind != "nested" {
			return errors.New("invalid coordinator record")
		}
	}
	seen := map[string]bool{}
	for _, k := range s.Order {
		if s.Tasks[k] == nil || seen[k] {
			return errors.New("invalid task order")
		}
		seen[k] = true
	}
	if len(seen) != len(s.Tasks) {
		return errors.New("incomplete task order")
	}
	for k, t := range s.Tasks {
		if t == nil || k != t.ID || t.Version < 1 || t.Text == "" || !strings.Contains("|queued|running|blocked|canceling|canceled|completed|failed|unknown|", "|"+t.State+"|") {
			return errors.New("invalid task record")
		}
	}
	sessions := map[string]bool{}
	for k, r := range s.Runs {
		if r == nil {
			return errors.New("nil run record")
		}
		t := s.Tasks[r.Task]
		if k != r.ID || t == nil || r.Version < 1 || r.Version > t.Version || r.Session == "" || sessions[r.Session] || !strings.Contains("|starting|running|canceling|stopped|failed|result_pending|accepted|discarded|unknown|", "|"+r.State+"|") {
			return errors.New("invalid run record")
		}
		sessions[r.Session] = true
	}
	for _, t := range s.Tasks {
		if t.State == "completed" {
			found := false
			for _, r := range s.Runs {
				if r.Task == t.ID && r.Version == t.Version && r.State == "accepted" && r.Evidence != "" {
					found = true
				}
			}
			if t.Evidence == "" || !found {
				return errors.New("completed task lacks current evidence")
			}
		}
	}
	for event, r := range s.Events {
		if event == "" || r.Request.Event != event || r.Request.Scope != s.Scope || r.Request.Owner != s.Owner || len(r.Fingerprint) != 64 {
			return errors.New("invalid input receipt")
		}
	}
	for task, n := range s.Notices {
		if s.Tasks[task] == nil || n.Revision < 1 || n.Revision > s.Revision || n.Version < 1 || n.Version > s.Tasks[task].Version {
			return errors.New("invalid key snapshot")
		}
	}
	a, q := s.counts()
	if a > s.Limits.Running || q > s.Limits.Queued {
		return errors.New("ledger exceeds hard limits")
	}
	return nil
}

// Apply serializes across processes using a kernel-owned lock, not a stale PID
// or expiring lease. An interrupted transaction is either old or new JSON.
func Apply(root string, q Request) (Response, error) {
	if !filepath.IsAbs(root) || !validScope(q.Scope) || q.Owner == "" {
		return Response{}, errors.New("absolute root, exact scope and owner required")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return Response{}, err
	}
	unlock, err := lock(filepath.Join(root, ".lock"))
	if err != nil {
		return Response{}, err
	}
	defer unlock()
	file := filepath.Join(root, Key(q.Scope)+".json")
	b, err := os.ReadFile(file)
	var s State
	if errors.Is(err, os.ErrNotExist) {
		if q.Op != "init" {
			return Response{}, errors.New("task scope not initialized")
		}
		limits := Limits{1, 8}
		if q.Limits != nil {
			limits = *q.Limits
		}
		mode := q.Mode
		if mode == "" {
			mode = "manual"
		}
		s = State{Schema: 1, Scope: q.Scope, Owner: q.Owner, Limits: limits, Mode: mode, Tasks: map[string]*Task{}, Order: []string{}, Runs: map[string]*Run{}, Events: map[string]Receipt{}, Notices: map[string]Notice{}}
	} else if err != nil {
		return Response{}, err
	} else if err = json.Unmarshal(b, &s); err != nil {
		return Response{}, fmt.Errorf("corrupt task ledger; refusing recovery: %w", err)
	}
	if err = s.validate(); err != nil {
		return Response{}, err
	}
	if s.Scope != q.Scope || s.Owner != q.Owner {
		return Response{}, errors.New("task scope owner mismatch")
	}
	if q.Op == "status" {
		return Response{Outcome: "ok", State: &s}, nil
	}
	s.headroom = capacity{s.Limits.Running, s.Limits.Queued, 1}
	if q.Op == "submit" || q.Op == "revise" || q.Op == "reserve" || q.Op == "coordinator" || q.Op == "coordinator_wake" {
		s.headroom, err = headroom(root, &s)
		if err != nil {
			return Response{}, err
		}
	}
	r, err := s.apply(q)
	if err != nil {
		return Response{}, err
	}
	if err = s.validate(); err != nil {
		return Response{}, err
	}
	data, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return Response{}, err
	}
	tmp, err := os.CreateTemp(root, ".transaction-")
	if err != nil {
		return Response{}, err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), file)
	}
	if err == nil {
		var d *os.File
		d, err = os.Open(root)
		if err == nil {
			err = d.Sync()
			d.Close()
		}
	}
	if err != nil {
		return Response{}, err
	}
	return r, nil
}
func (s *State) apply(q Request) (Response, error) {
	r := Response{Outcome: "ok"}
	t := s.Tasks[q.Task]
	// Event identities, not content, deduplicate input. A reused identity with
	// different payload is an error; rejected inputs remain visible receipts.
	eventOp := q.Op == "submit" || q.Op == "revise" || q.Op == "cancel" || q.Op == "notify" || q.Op == "coordinator_wake"
	var fingerprint string
	if eventOp {
		if q.Event == "" {
			return r, errors.New("event id required")
		}
		copy := q
		copy.Event = ""
		b, _ := json.Marshal(copy)
		h := sha256.Sum256(b)
		fingerprint = hex.EncodeToString(h[:])
		if old, ok := s.Events[q.Event]; ok {
			if old.Fingerprint != fingerprint {
				return r, errors.New("event id reused with different request")
			}
			return Response{Outcome: old.Outcome, Task: s.Tasks[old.Task], Replayed: true}, nil
		}
	}
	switch q.Op {
	case "init", "status":
		r.State = s
	case "coordinator":
		if s.Halted || s.Mode != "three_level" {
			return Response{Outcome: "unavailable", State: s}, nil
		}
		if s.Coordinator != nil {
			return Response{Outcome: "existing", Coordinator: s.Coordinator}, nil
		}
		if s.headroom.Coordinators < 1 {
			return Response{Outcome: "coordinator_capacity"}, nil
		}
		s.Coordinator = &Coordinator{Session: "jand-c-" + id(), State: "starting", Kind: q.Mode}
		r.Outcome = "reserved"
		r.Coordinator = s.Coordinator
	case "coordinator_wake":
		if s.Halted || s.headroom.Coordinators < 1 && s.Coordinator != nil && s.Coordinator.State == "stopped" || s.Coordinator == nil || s.Coordinator.Kind != "independent" || (s.Coordinator.State != "idle" && s.Coordinator.State != "stopped") {
			r.Outcome = "not_admitted"
			break
		}
		s.Coordinator.State = "starting"
		r.Outcome = "admitted"
		r.Coordinator = s.Coordinator
	case "coordinator_observe":
		if s.Coordinator == nil {
			return r, errors.New("no coordinator")
		}
		if q.Status != "running" && q.Status != "stopped" && q.Status != "unknown" && q.Status != "canceling" && q.Status != "idle" {
			return r, errors.New("invalid coordinator observation")
		}
		if s.Halted && (q.Status == "running" || q.Status == "idle") {
			return Response{Outcome: "cancel_required", Coordinator: s.Coordinator}, nil
		}
		if s.Coordinator.State == "stopped" && q.Status != "stopped" {
			return Response{Outcome: "terminal", Coordinator: s.Coordinator}, nil
		}
		s.Coordinator.State = q.Status
		r.Coordinator = s.Coordinator
	case "submit":
		_, queued := s.counts()
		if s.Halted {
			r.Outcome = "halted"
		} else if queued >= s.Limits.Queued || queued >= s.headroom.Queued {
			r.Outcome = "full"
		} else {
			t = &Task{ID: id(), Version: 1, Text: q.Text, State: "queued"}
			if t.Text == "" {
				return r, errors.New("task text required")
			}
			s.Tasks[t.ID] = t
			s.Order = append(s.Order, t.ID)
			r.Outcome = "queued"
			r.Task = t
			s.notice(t)
		}
	case "notify":
		if t == nil {
			return r, errors.New("unknown task")
		}
		s.notice(t)
		r.Task = t
		r.Outcome = "snapshot_updated"
	case "revise", "cancel":
		if t == nil || t.Version != q.Version {
			return r, errors.New("task version mismatch")
		}
		if q.Op == "revise" {
			if s.Halted {
				return r, errors.New("dispatch halted")
			}
			if q.Text == "" {
				return r, errors.New("replacement text required")
			}
			_, queued := s.counts()
			if t.State != "queued" && !(t.State == "blocked" && t.Evidence == "awaiting_prior_stop") && (queued >= s.Limits.Queued || queued >= s.headroom.Queued) {
				r.Outcome = "full"
				break
			}
			t.Version++
			t.Text = q.Text
			t.State = "queued"
			t.Evidence = ""
		} else {
			t.State = "canceled"
			t.Evidence = ""
		}
		for _, run := range s.Runs {
			if run.Task == t.ID && run.State == "result_pending" {
				run.State = "discarded"
			}
			if run.Task == t.ID && active(run.State) {
				run.State = "canceling"
				if q.Op == "revise" {
					t.State = "blocked"
					t.Evidence = "awaiting_prior_stop"
				} else {
					t.State = "canceling"
				}
			}
		}
		r.Task = t
		s.notice(t)
	case "reserve":
		if s.Halted {
			return Response{Outcome: "halted"}, nil
		}
		if s.Mode == "manual" {
			return Response{Outcome: "manual_required"}, nil
		}
		running, _ := s.counts()
		if running >= s.Limits.Running || running >= s.headroom.Running {
			return Response{Outcome: "capacity"}, nil
		}
		// Unknown starts block ALL dispatch: they may have untracked descendants.
		for _, run := range s.Runs {
			if run.State == "unknown" || run.State == "canceling" {
				return Response{Outcome: "reconcile_required"}, nil
			}
		}
		for _, k := range s.Order {
			candidate := s.Tasks[k]
			if candidate.State == "queued" && !s.outstanding(k) {
				t = candidate
				break
			}
			t = nil
		}
		if t == nil {
			return Response{Outcome: "empty"}, nil
		}
		run := &Run{ID: id(), Task: t.ID, Version: t.Version, Session: "jand-e-" + id(), State: "starting"}
		s.Runs[run.ID] = run
		t.State = "running"
		s.notice(t)
		r = Response{Outcome: "reserved", Run: run, Task: t}
	case "observe":
		run := s.Runs[q.Run]
		if run == nil {
			return r, errors.New("unknown run")
		}
		t = s.Tasks[run.Task]
		if !active(run.State) {
			return Response{Outcome: "terminal", Run: run, Task: t}, nil
		}
		if q.Status != "running" && q.Status != "stopped" && q.Status != "failed" && q.Status != "unknown" && q.Status != "result_pending" {
			return r, errors.New("invalid run observation")
		}
		if q.Status == "result_pending" && q.Evidence == "" {
			return r, errors.New("result evidence required")
		}
		if q.Status == "running" && (run.State == "canceling" || s.Halted || run.Version != t.Version) {
			return Response{Outcome: "cancel_required", Run: run, Task: t}, nil
		}
		if q.Status == "result_pending" && (run.State == "canceling" || s.Halted || run.Version != t.Version) {
			return Response{Outcome: "stop_confirmation_required", Run: run, Task: t}, nil
		}
		run.State = q.Status
		run.Evidence = q.Evidence
		if run.Version == t.Version {
			switch q.Status {
			case "running":
				t.State = "running"
			case "stopped":
				if t.Evidence == "paused" {
					t.State = "blocked"
				} else if t.State == "canceling" || s.Halted {
					t.State = "canceled"
				} else {
					t.State = "blocked"
				}
			case "failed":
				t.State = "failed"
			case "unknown":
				t.State = "unknown"
			case "result_pending":
				t.State = "blocked"
				t.Evidence = "awaiting_review"
			}
		} else if !s.outstanding(t.ID) && t.State == "blocked" && (t.Evidence == "awaiting_prior_stop" || t.Evidence == "paused") {
			t.State = "queued"
			t.Evidence = ""
		}
		s.notice(t)
		r.Run = run
		r.Task = t
	case "accept":
		run := s.Runs[q.Run]
		if t == nil || run == nil || run.Task != t.ID || run.Version != t.Version || q.Version != t.Version || run.State != "result_pending" || q.Evidence == "" || t.State != "blocked" || t.Evidence != "awaiting_review" {
			return r, errors.New("current result and acceptance evidence required")
		}
		t.State = "completed"
		run.State = "accepted"
		t.Evidence = q.Evidence
		r.Task = t
		s.notice(t)
	case "stop", "pause":
		s.ControlID++
		s.Halted = true
		for _, t := range s.Tasks {
			if q.Op == "stop" && (t.State == "queued" || t.State == "blocked") {
				t.State = "canceled"
			}
			if s.outstanding(t.ID) {
				t.State = "canceling"
				if q.Op == "pause" {
					t.Evidence = "paused"
				}
			}
			s.notice(t)
		}
		for _, run := range s.Runs {
			if run.State == "result_pending" && q.Op == "stop" {
				run.State = "discarded"
			}
			if active(run.State) {
				run.State = "canceling"
			}
		}
		if s.Coordinator != nil && s.Coordinator.State != "stopped" {
			s.Coordinator.State = "canceling"
		}
		r.Outcome = "cancel_requested"
		r.State = s
	case "resume":
		a := 0
		for _, run := range s.Runs {
			if active(run.State) {
				a++
			}
		}
		if a > 0 || s.Coordinator != nil && s.Coordinator.State != "stopped" {
			return r, errors.New("stop not confirmed; reconcile before resume")
		}
		s.Halted = false
		r.State = s
	case "notice_ack":
		// Only exact host processing receipts qualify. The adapter deliberately
		// exposes no automatic acknowledgement using recv/idle/queue state.
		if q.Evidence == "" {
			return r, errors.New("host processing receipt required")
		}
		n, ok := s.Notices[q.Task]
		if ok && n.Revision == q.Revision {
			delete(s.Notices, q.Task)
		} else {
			r.Outcome = "stale_receipt"
		}
	default:
		return r, errors.New("unknown task operation")
	}
	if eventOp {
		tid := q.Task
		if r.Task != nil {
			tid = r.Task.ID
		}
		s.Events[q.Event] = Receipt{Request: q, Fingerprint: fingerprint, Outcome: r.Outcome, Task: tid}
	}
	return r, nil
}
