package transfer

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jaredchao/jand/internal/relay"
)

func TestParsePlan(t *testing.T) {
	md := "计划：\n\n- 查内存\n* 汇总成报告，\n  写明来源\n\n1. 列出未决\n2) 收尾\n不是列表的行\n"
	steps, err := ParsePlan([]byte(md))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"查内存", "汇总成报告， 写明来源", "列出未决", "收尾"}
	if len(steps) != len(want) {
		t.Fatalf("markdown plan: %+v", steps)
	}
	for i, w := range want {
		if steps[i].Goal != w || steps[i].Budget != 0 {
			t.Fatalf("step %d: %+v", i, steps[i])
		}
	}
	steps, err = ParsePlan([]byte(` [{"goal":"查内存","budget":5},{"goal":"汇总"}]`))
	if err != nil || len(steps) != 2 || steps[0].Budget != 5 || steps[1].Goal != "汇总" {
		t.Fatalf("json plan: %+v %v", steps, err)
	}
	long := strings.Repeat("长", MaxChatGoal)
	for name, bad := range map[string]string{
		"empty":         "只有说明，没有列表\n",
		"unknown field": `[{"goal":"x","owner":"me"}]`,
		"too long":      "- " + long,
		"too many":      strings.Repeat("- 一步\n", MaxPlanSteps+1),
		"bad budget":    `[{"goal":"x","budget":999}]`,
	} {
		if _, err := ParsePlan([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseAdvance(t *testing.T) {
	for in, want := range map[string]string{"": "ask", "ask": "ask", "auto": "auto", "5m": "5m", "300s": "5m", "90s": "1m30s", "2h": "2h", "1h30m": "1h30m"} {
		if got, err := ParseAdvance(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"30s", "7h", "soon", "-5m"} {
		if _, err := ParseAdvance(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// planPair starts a chat with a plan and has the guest join; each side's
// advance mode is its own.
func planPair(t *testing.T, hostAdvance, guestAdvance string) (context.Context, *side, *side, string) {
	t.Helper()
	_, srv := server(t, relay.DefaultConfig())
	ctx := context.Background()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	o := withChat(host.o)
	o.Budget, o.Advance = 10, hostAdvance
	o.Plan = []PlanStep{{Goal: "查内存"}, {Goal: "汇总成报告", Budget: 6}}
	if err := Send(ctx, fixture(t, []byte("x")), o); err != nil {
		t.Fatal(err)
	}
	id, rawCode := host.events[0].Chat, host.events[0].Code
	if _, err := Receive(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	saved := guest.events[len(guest.events)-1]
	if len(saved.Plan) != 2 || saved.Plan[0] != (PlanStep{Goal: "查内存", Budget: 10}) || saved.Plan[1].Budget != 6 {
		t.Fatalf("the guest's user must see the plan, with the budgets the host will use: %+v", saved.Plan)
	}
	g := guest.o
	g.Advance = guestAdvance
	guest.events = nil
	if _, err := ChatJoin(ctx, rawCode, g); err != nil {
		t.Fatal(err)
	}
	if j := guest.events[0]; len(j.Plan) != 2 || j.Advance != guestAdvance {
		t.Fatalf("joined: %+v", j)
	}
	host.recv(ctx, id, 0)
	return ctx, host, guest, id
}

// bothDone brings the chat to an all_done checkpoint and returns what each
// side's recv then gives.
func bothDone(t *testing.T, ctx context.Context, host, guest *side, id string) (hostEv, guestEv []Event) {
	t.Helper()
	for _, s := range []*side{host, guest} {
		if err := ChatDone(ctx, id, "", s.o); err != nil {
			t.Fatal(err)
		}
	}
	hostEv, _ = host.recv(ctx, id, time.Second)
	guestEv, _ = guest.recv(ctx, id, time.Second)
	return
}

func TestChatPlanAdvancesOnlyToThePlannedGoal(t *testing.T) {
	ctx, host, guest, id := planPair(t, "auto", "auto")

	// Goal 1 done. auto: the host may propose goal 2 at once.
	hev, _ := bothDone(t, ctx, host, guest, id)
	if types(hev) != "done,checkpoint,advance_due" || hev[1].AdvanceAt == "" || hev[2].PlanStep != 2 || hev[2].Goal != "查内存" {
		t.Fatalf("host at checkpoint: %+v", hev)
	}
	if ev, _ := host.recv(ctx, id, 0); types(ev) != "no_events" {
		t.Fatalf("advance_due repeated: %v", types(ev))
	}
	// The guest has nothing to accept until the host proposes.
	if err := ChatAdvance(ctx, id, false, guest.o); err == nil || !strings.Contains(err.Error(), "not proposed") {
		t.Fatalf("guest advanced with no proposal: %v", err)
	}
	host.events = nil
	if err := ChatAdvance(ctx, id, false, host.o); err != nil {
		t.Fatal(err)
	}
	if types(host.events) != "proposed,advanced" || host.events[1].PlanStep != 2 {
		t.Fatalf("host advance: %+v", host.events)
	}
	if err := ChatAdvance(ctx, id, false, host.o); err == nil {
		t.Fatal("proposed the same step twice")
	}
	gev, _ := guest.recv(ctx, id, time.Second)
	if types(gev) != "proposal,advance_due" || gev[0].PlanStep != 2 || !gev[0].Untrusted || gev[1].PlanStep != 2 {
		t.Fatalf("guest sees the planned proposal: %+v", gev)
	}
	if err := ChatAdvance(ctx, id, false, guest.o); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*side{host, guest} {
		ev, _ := s.recv(ctx, id, time.Second)
		if types(ev) != "resumed" || ev[0].Goal != "查内存" || ev[0].Budget != 10 || ev[0].PlanStep != 2 {
			t.Fatalf("resumed on goal 2: %+v", ev)
		}
	}

	// Goal 2 done. The host's user proposes something outside the plan: the
	// guest's jand does not recognise it, and nothing is automatic.
	bothDone(t, ctx, host, guest, id)
	if err := ChatPropose(ctx, id, "顺便查磁盘", 0, host.o); err != nil {
		t.Fatal(err)
	}
	if err := ChatAdvance(ctx, id, false, host.o); err == nil {
		t.Fatal("host advanced after its user proposed")
	}
	gev, _ = guest.recv(ctx, id, time.Second)
	if types(gev) != "proposal" || gev[0].PlanStep != 0 {
		t.Fatalf("an unplanned goal must wait for the user: %+v", gev)
	}
	if err := ChatAdvance(ctx, id, false, guest.o); err == nil {
		t.Fatal("guest accepted an unplanned goal without its user")
	}
	if err := ChatAccept(ctx, id, guest.o); err != nil { // the user agreed
		t.Fatal(err)
	}
	host.recv(ctx, id, time.Second)
	guest.recv(ctx, id, time.Second)

	// The unplanned goal did not use up the plan: goal 3 is still next.
	hev, _ = bothDone(t, ctx, host, guest, id)
	if last := hev[len(hev)-1]; last.Event != "advance_due" || last.Goal != "汇总成报告" || last.PlanStep != 3 {
		t.Fatalf("plan after an unplanned goal: %+v", hev)
	}
	// The host proposes goal 3 with a different budget: not the plan's goal.
	if err := ChatPropose(ctx, id, "汇总成报告", 9, host.o); err != nil {
		t.Fatal(err)
	}
	if gev, _ = guest.recv(ctx, id, time.Second); gev[len(gev)-1].PlanStep != 0 {
		t.Fatalf("a changed budget passed as planned: %+v", gev)
	}
	// Proposed word for word, it is: propose --next uses NextPlanStep.
	step, n, err := NextPlanStep(id, host.o)
	if err != nil || n != 3 || step.Budget != 6 {
		t.Fatalf("next step: %+v %d %v", step, n, err)
	}
	if err := ChatPropose(ctx, id, step.Goal, step.Budget, host.o); err != nil {
		t.Fatal(err)
	}
	if gev, _ = guest.recv(ctx, id, time.Second); types(gev) != "proposal,advance_due" || gev[0].PlanStep != 3 {
		t.Fatalf("goal 3 word for word: %+v", gev)
	}
	if err := ChatAdvance(ctx, id, false, guest.o); err != nil {
		t.Fatal(err)
	}
	host.recv(ctx, id, time.Second)
	guest.recv(ctx, id, time.Second)

	// The plan is finished: the next checkpoint asks the users.
	hev, _ = bothDone(t, ctx, host, guest, id)
	if types(hev) != "done,checkpoint" || hev[1].AdvanceAt != "" {
		t.Fatalf("after the plan: %+v", hev)
	}
	if err := ChatAdvance(ctx, id, false, host.o); err == nil || !strings.Contains(err.Error(), "finished") {
		t.Fatalf("advanced past the plan: %v", err)
	}

	// The transcript keeps what was taken without asking.
	data, _ := os.ReadFile(guestTranscript(guest, id))
	var advanced []TranscriptLine
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var l TranscriptLine
		json.Unmarshal([]byte(line), &l)
		if l.Kind == "joined" && len(l.Plan) != 2 {
			t.Fatalf("joined line lacks the plan: %s", line)
		}
		if l.Kind == "advanced" {
			advanced = append(advanced, l)
		}
	}
	if len(advanced) != 2 || advanced[0].Step != 2 || advanced[1].Step != 3 || advanced[0].Mode != "auto" {
		t.Fatalf("advanced lines: %+v", advanced)
	}
}

func guestTranscript(s *side, id string) string {
	return s.o.StateDir + "/" + id + ".transcript.jsonl"
}

func TestChatPlanWaitsForTheUserFirst(t *testing.T) {
	ctx, host, guest, id := planPair(t, "1m", "ask")
	hev, gev := bothDone(t, ctx, host, guest, id)
	if types(hev) != "done,checkpoint" || hev[1].AdvanceAt == "" {
		t.Fatalf("host checkpoint with a wait: %+v", hev)
	}
	if at, _ := time.Parse(time.RFC3339, hev[1].AdvanceAt); time.Until(at) < 50*time.Second {
		t.Fatalf("advance_at %s is not a minute away", hev[1].AdvanceAt)
	}
	if gev[len(gev)-1].AdvanceAt != "" {
		t.Fatalf("an ask side gets no advance time: %+v", gev)
	}
	if err := ChatAdvance(ctx, id, false, host.o); err == nil || !strings.Contains(err.Error(), "not yet") {
		t.Fatalf("advanced before the user's time was up: %v", err)
	}
	s, _ := loadChat(host.o.StateDir, id)

	// recv wakes by itself when the wait is over, without the agent timing it.
	s.updatePlan(host.o.StateDir, func(p *planState) { p.PausedAt = time.Now().Unix() - 59 })
	start := time.Now()
	ev, err := host.recv(ctx, id, 30*time.Second)
	if err != nil || types(ev) != "advance_due" || time.Since(start) > 5*time.Second {
		t.Fatalf("recv did not wake for advance_due: %v %v after %s", types(ev), err, time.Since(start))
	}
	if err := ChatAdvance(ctx, id, false, host.o); err != nil {
		t.Fatal(err)
	}
	// The guest's user said ask: the planned goal is marked, never taken.
	gev, _ = guest.recv(ctx, id, time.Second)
	if types(gev) != "proposal" || gev[0].PlanStep != 2 {
		t.Fatalf("ask side: %+v", gev)
	}
	if err := ChatAdvance(ctx, id, false, guest.o); err == nil || !strings.Contains(err.Error(), "ask") {
		t.Fatalf("an ask side advanced: %v", err)
	}
}

func TestChatPlanAdvanceOff(t *testing.T) {
	ctx, host, guest, id := planPair(t, "auto", "auto")
	if err := ChatAdvance(ctx, id, true, host.o); err != nil {
		t.Fatal(err)
	}
	hev, _ := bothDone(t, ctx, host, guest, id)
	if types(hev) != "done,checkpoint" || hev[1].AdvanceAt != "" {
		t.Fatalf("advance after off: %+v", hev)
	}
	if err := ChatAdvance(ctx, id, false, host.o); err == nil || !strings.Contains(err.Error(), "off") {
		t.Fatalf("advanced after off: %v", err)
	}
}

func TestPlanNeedsChat(t *testing.T) {
	o := Options{RelayURL: "http://127.0.0.1:1", Plan: []PlanStep{{Goal: "x"}}}
	if err := Send(context.Background(), fixture(t, []byte("x")), o); err == nil || !strings.Contains(err.Error(), "--chat") {
		t.Fatalf("plan without chat: %v", err)
	}
}
