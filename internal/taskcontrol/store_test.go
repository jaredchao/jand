//go:build darwin || linux || freebsd

package taskcontrol

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var scope = Scope{"dsh", "local-a", "chat-1", "phase-1"}

func request(op string) Request { return Request{Op: op, Scope: scope, Owner: "main"} }
func call(t *testing.T, root string, q Request) Response {
	t.Helper()
	r, e := Apply(root, q)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func initStore(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	q := request("init")
	q.Mode = "three_level"
	call(t, root, q)
	return root
}
func submit(t *testing.T, root, event, text string) Response {
	t.Helper()
	q := request("submit")
	q.Event = event
	q.Text = text
	return call(t, root, q)
}
func TestIdentityCapacityAndEventSemantics(t *testing.T) {
	root := initStore(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := map[string]bool{}
	full := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := request("submit")
			q.Event = id()
			q.Text = "same legitimate requirement"
			r, e := Apply(root, q)
			if e != nil {
				t.Error(e)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if r.Outcome == "full" {
				full++
			} else {
				ids[r.Task.ID] = true
			}
		}()
	}
	wg.Wait()
	if len(ids) != 8 || full != 24 {
		t.Fatalf("tasks %d full %d", len(ids), full)
	}
	s := call(t, root, request("status")).State
	if len(s.Events) != 32 {
		t.Fatal("rejected inputs lost")
	}
	var rejected Request
	for _, event := range s.Events {
		if event.Outcome == "full" {
			rejected = event.Request
			break
		}
	}
	if rejected.Text != "same legitimate requirement" {
		t.Fatal("full input not retained")
	}
	if call(t, root, rejected).Outcome != "full" {
		t.Fatal("duplicate rejection changed")
	}
	rejected.Text = "changed"
	if _, e := Apply(root, rejected); e == nil {
		t.Fatal("event collision accepted")
	}
	wg = sync.WaitGroup{}
	n := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := Apply(root, request("reserve"))
			if e != nil {
				t.Error(e)
				return
			}
			if r.Outcome == "reserved" {
				mu.Lock()
				n++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if n != 1 {
		t.Fatalf("%d dispatches", n)
	}
}
func TestRevisionCancelAndEvidence(t *testing.T) {
	root := initStore(t)
	task := submit(t, root, "a", "v1").Task
	run := call(t, root, request("reserve")).Run
	q := request("revise")
	q.Task = task.ID
	q.Version = 1
	q.Text = "v2"
	q.Event = "edit"
	call(t, root, q)
	if r := call(t, root, request("reserve")); r.Outcome == "reserved" {
		t.Fatal("new version overlaps old")
	}
	q = request("observe")
	q.Run = run.ID
	q.Status = "result_pending"
	q.Evidence = "stale result"
	if call(t, root, q).Outcome != "stop_confirmation_required" {
		t.Fatal("stale result accepted")
	}
	q.Status = "unknown"
	call(t, root, q)
	if call(t, root, request("reserve")).Outcome == "reserved" {
		t.Fatal("unknown reused")
	}
	q.Status = "stopped"
	call(t, root, q)
	newRun := call(t, root, request("reserve")).Run
	if newRun.Version != 2 {
		t.Fatal("version lost")
	}
	q = request("accept")
	q.Task = task.ID
	q.Version = 2
	q.Run = run.ID
	q.Evidence = "claim"
	if _, e := Apply(root, q); e == nil {
		t.Fatal("old return completed current version")
	}
	q = request("observe")
	q.Run = newRun.ID
	q.Status = "result_pending"
	q.Evidence = "artifact hash"
	call(t, root, q)
	if call(t, root, request("status")).State.Tasks[task.ID].State != "blocked" {
		t.Fatal("exit treated as acceptance")
	}
	q = request("accept")
	q.Task = task.ID
	q.Version = 2
	q.Run = newRun.ID
	q.Evidence = "reviewed artifact"
	call(t, root, q)
	q = request("cancel")
	q.Task = task.ID
	q.Version = 2
	q.Event = "cancel"
	call(t, root, q)
	q = request("accept")
	q.Task = task.ID
	q.Version = 2
	q.Run = newRun.ID
	q.Evidence = "late"
	if _, e := Apply(root, q); e == nil {
		t.Fatal("canceled task resurrected")
	}
}
func TestStopPriorityAndMergedNotices(t *testing.T) {
	root := initStore(t)
	task := submit(t, root, "a", "task").Task
	run := call(t, root, request("reserve")).Run
	for i := 0; i < 100; i++ {
		q := request("notify")
		q.Event = id()
		q.Task = task.ID
		call(t, root, q)
	}
	s := call(t, root, request("status")).State
	if len(s.Notices) != 1 {
		t.Fatal("notice backlog")
	}
	old := s.Notices[task.ID]
	call(t, root, request("stop"))
	q := request("notice_ack")
	q.Task = task.ID
	q.Revision = old.Revision
	q.Evidence = "processed"
	if call(t, root, q).Outcome != "stale_receipt" {
		t.Fatal("old ack removed newer state")
	}
	if call(t, root, request("reserve")).Outcome != "halted" {
		t.Fatal("dispatch after stop")
	}
	if _, e := Apply(root, request("resume")); e == nil {
		t.Fatal("unconfirmed stop resumed")
	}
	if submit(t, root, "later", "keep input").Outcome != "halted" {
		t.Fatal("stopped input admitted")
	}
	q = request("observe")
	q.Run = run.ID
	q.Status = "unknown"
	q.Evidence = "cancel failed"
	call(t, root, q)
	s = call(t, root, request("status")).State
	if s.Tasks[task.ID].State != "unknown" || !s.Halted {
		t.Fatal("cancel failure hidden")
	}
	q.Status = "stopped"
	q.Evidence = "native disposed"
	call(t, root, q)
	call(t, root, request("resume"))
}
func TestCorruptAndUnsupportedNeverReset(t *testing.T) {
	root := initStore(t)
	file := filepath.Join(root, Key(scope)+".json")
	os.WriteFile(file, []byte(`{"schema":1}`), 0600)
	if _, e := Apply(root, request("init")); e == nil {
		t.Fatal("corrupt ledger reset")
	}
	data, _ := os.ReadFile(file)
	if string(data) != `{"schema":1}` {
		t.Fatal("corruption overwritten")
	}
}

// The helper uses a separate OS process per contender: mutexes and in-memory
// maps cannot make this test pass. Crashed helpers release the OS lock.
func TestProcessHelper(t *testing.T) {
	if os.Getenv("JAND_TASK_HELPER") != "1" {
		return
	}
	root := os.Getenv("JAND_TASK_ROOT")
	q := request(os.Getenv("JAND_TASK_OP"))
	r, e := Apply(root, q)
	if e != nil {
		os.Exit(3)
	}
	json.NewEncoder(os.Stdout).Encode(r)
	os.Exit(0)
}
func TestCrossProcessCoordinatorAndDispatchRestart(t *testing.T) {
	root := initStore(t)
	submit(t, root, "a", "task")
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	identities := map[string]bool{}
	helper := func(op string) Response {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
		cmd.Env = append(os.Environ(), "JAND_TASK_HELPER=1", "JAND_TASK_ROOT="+root, "JAND_TASK_OP="+op)
		b, e := cmd.Output()
		if e != nil {
			t.Error(e)
			return Response{}
		}
		var r Response
		if e = json.Unmarshal(b, &r); e != nil {
			t.Error(e)
		}
		return r
	}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := helper("coordinator")
			mu.Lock()
			defer mu.Unlock()
			if r.Outcome == "reserved" {
				created++
			}
			if r.Coordinator != nil {
				identities[r.Coordinator.Session] = true
			}
		}()
	}
	wg.Wait()
	if created != 1 || len(identities) != 1 {
		t.Fatalf("coordinators %d / %d", created, len(identities))
	}
	first := helper("reserve")
	if first.Run == nil {
		t.Fatal("no first dispatch")
	}
	if helper("reserve").Outcome == "reserved" {
		t.Fatal("restart redispatched uncertain start")
	}
	if helper("coordinator").Outcome != "existing" {
		t.Fatal("restart recreated manager")
	}
}

func TestHostWideCapacityAndIdentityIsolation(t *testing.T) {
	root := initStore(t)
	a := submit(t, root, "input", "A").Task
	call(t, root, request("coordinator"))
	call(t, root, request("reserve"))
	other := scope
	other.Phase = "other-phase"
	q := request("init")
	q.Scope = other
	q.Mode = "three_level"
	call(t, root, q)
	q.Op = "coordinator"
	if call(t, root, q).Outcome != "coordinator_capacity" {
		t.Fatal("host-wide coordinator cap bypassed")
	}
	q.Op = "submit"
	q.Event = "different-request"
	q.Text = "A"
	if call(t, root, q).Task.ID == a.ID {
		t.Fatal("goal text guessed identity")
	}
	q.Op = "reserve"
	q.Event = ""
	if call(t, root, q).Outcome != "capacity" {
		t.Fatal("host-wide dispatch cap bypassed")
	}
	for i := 0; i < 7; i++ {
		q.Op = "submit"
		q.Event = id()
		call(t, root, q)
	}
	q.Event = "visible-overflow"
	r := call(t, root, q)
	if r.Outcome != "full" {
		t.Fatal("host-wide queue cap bypassed")
	}
}
func TestCoordinatorWakeIsSingleDurableAdmission(t *testing.T) {
	root := initStore(t)
	q := request("coordinator")
	q.Mode = "independent"
	call(t, root, q)
	q = request("coordinator_observe")
	q.Status = "idle"
	call(t, root, q)
	q = request("coordinator_wake")
	q.Event = "local-wake"
	first := call(t, root, q)
	again := call(t, root, q)
	if first.Outcome != "admitted" || first.Replayed || !again.Replayed {
		t.Fatal("wake retry could be mistaken for new dispatch")
	}
	q.Event = "another-wake"
	if call(t, root, q).Outcome != "not_admitted" {
		t.Fatal("pending coordinator notification duplicated")
	}
	call(t, root, request("stop"))
	q = request("coordinator_observe")
	q.Status = "running"
	if call(t, root, q).Outcome != "cancel_required" {
		t.Fatal("stale owner invalidated cutoff")
	}
}
func TestPendingEvidenceKeepsSlotAndPauseDoesNotCloseQueue(t *testing.T) {
	root := initStore(t)
	submit(t, root, "a", "A")
	submit(t, root, "b", "B")
	run := call(t, root, request("reserve")).Run
	q := request("observe")
	q.Run = run.ID
	q.Status = "result_pending"
	q.Evidence = "actual artifact"
	call(t, root, q)
	if call(t, root, request("reserve")).Outcome != "capacity" {
		t.Fatal("unreviewed results can accumulate without bound")
	}
	call(t, root, request("pause"))
	s := call(t, root, request("status")).State
	queued := 0
	for _, task := range s.Tasks {
		if task.State == "queued" {
			queued++
		}
	}
	if queued != 1 || !s.Halted {
		t.Fatal("pause lost queued work")
	}
	call(t, root, request("resume"))
	q = request("accept")
	q.Task = run.Task
	q.Version = run.Version
	q.Run = run.ID
	q.Evidence = "review"
	call(t, root, q)
	if call(t, root, request("reserve")).Run == nil {
		t.Fatal("acceptance did not release slot")
	}
}
func TestSemanticCorruptionDoesNotBecomeSuccessfulRecovery(t *testing.T) {
	root := initStore(t)
	task := submit(t, root, "a", "task").Task
	file := filepath.Join(root, Key(scope)+".json")
	data, _ := os.ReadFile(file)
	var s State
	json.Unmarshal(data, &s)
	s.Tasks[task.ID].State = "completed"
	data, _ = json.Marshal(s)
	os.WriteFile(file, data, 0600)
	if _, e := Apply(root, request("status")); e == nil {
		t.Fatal("completed task without actual result accepted")
	}
	after, _ := os.ReadFile(file)
	if string(after) != string(data) {
		t.Fatal("corruption was overwritten")
	}
}
func TestPausedExecutionRequiresExplicitRevision(t *testing.T) {
	root := initStore(t)
	task := submit(t, root, "a", "A").Task
	run := call(t, root, request("reserve")).Run
	call(t, root, request("pause"))
	q := request("observe")
	q.Run = run.ID
	q.Status = "stopped"
	q.Evidence = "actual exit"
	call(t, root, q)
	call(t, root, request("resume"))
	if call(t, root, request("reserve")).Outcome == "reserved" {
		t.Fatal("paused partial work silently repeated")
	}
	q = request("revise")
	q.Task = task.ID
	q.Version = 1
	q.Event = "local-revision"
	q.Text = "inspect partial work and continue safely"
	call(t, root, q)
	if call(t, root, request("reserve")).Run.Version != 2 {
		t.Fatal("explicit revision not admitted")
	}
}
func TestPauseWithFullQueueKeepsConfirmationAndRequiresDecision(t *testing.T) {
	root := initStore(t)
	task := submit(t, root, "running", "A").Task
	run := call(t, root, request("reserve")).Run
	for i := 0; i < 8; i++ {
		submit(t, root, id(), "queued")
	}
	call(t, root, request("pause"))
	q := request("observe")
	q.Run = run.ID
	q.Status = "stopped"
	q.Evidence = "native and process group gone"
	call(t, root, q)
	call(t, root, request("resume"))
	if call(t, root, request("reserve")).Outcome != "capacity" {
		t.Fatal("interrupted work decision lost")
	}
	q = request("cancel")
	q.Task = task.ID
	q.Version = 1
	q.Event = "local-discard-partial"
	call(t, root, q)
	if call(t, root, request("reserve")).Run == nil {
		t.Fatal("canceling interruption did not release capacity")
	}
}
