package transfer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"net/http/httputil"
	"net/url"

	"github.com/jaredchao/jand/internal/code"
	"github.com/jaredchao/jand/internal/relay"
)

type side struct {
	t      *testing.T
	o      Options
	events []Event
}

func newSide(t *testing.T, url string) *side {
	s := &side{t: t}
	s.o = Options{RelayURL: url, OutputDir: t.TempDir(), StateDir: t.TempDir(), Emit: func(e Event) { s.events = append(s.events, e) }}
	return s
}

// recv returns the events one ChatRecv call emitted.
func (s *side) recv(ctx context.Context, id string, wait time.Duration) ([]Event, error) {
	s.events = nil
	err := ChatRecv(ctx, id, wait, nil, s.o)
	return s.events, err
}

func types(events []Event) string {
	var out []string
	for _, e := range events {
		out = append(out, e.Event)
	}
	return strings.Join(out, ",")
}

// invite sends a --chat packet from host and has guest claim it.
func invite(t *testing.T, ctx context.Context, host, guest *side) (chatID, rawCode string) {
	t.Helper()
	if err := Send(ctx, fixture(t, []byte("# 交接\n联调接口")), withChat(host.o)); err != nil {
		t.Fatal(err)
	}
	queued := host.events[0]
	if queued.Event != "queued" || len(queued.Chat) != 32 {
		t.Fatalf("queued: %+v", queued)
	}
	if _, err := Receive(ctx, queued.Code, guest.o); err != nil {
		t.Fatal(err)
	}
	saved := guest.events[len(guest.events)-1]
	if saved.Event != "saved" || !saved.ChatInvite || saved.Chat != "" || saved.Goal != testGoal || !saved.RequiresUserApproval {
		t.Fatalf("saved: %+v", saved)
	}
	return queued.Chat, queued.Code
}

const testGoal = "双方对 /users 响应字段达成一致"

func withChat(o Options) Options {
	o.Chat, o.Goal = true, testGoal
	return o
}

func TestChatConversation(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	id, rawCode := invite(t, ctx, host, guest)

	if ev, err := host.recv(ctx, id[:8], 0); err != nil || types(ev) != "opened" {
		t.Fatalf("host after claim: %v %v", types(ev), err)
	}
	if err := ChatSend(ctx, id, ChatMessage{Text: "too early"}, host.o); err == nil || !strings.Contains(err.Error(), "peer has not joined") {
		t.Fatalf("send before join: %v", err)
	}
	if _, err := ChatJoin(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	if _, err := ChatJoin(ctx, rawCode, newSide(t, srv.URL).o); err == nil || !strings.Contains(err.Error(), "already joined") {
		t.Fatalf("second join with the same code: %v", err)
	}
	if ev, _ := host.recv(ctx, id, 0); types(ev) != "joined" {
		t.Fatalf("host sees join: %v", types(ev))
	}

	if err := ChatSend(ctx, id, ChatMessage{Text: "GET /users 返回什么结构？"}, host.o); err != nil {
		t.Fatal(err)
	}
	ev, err := guest.recv(ctx, id, time.Second)
	if err != nil || types(ev) != "message" || ev[0].Text != "GET /users 返回什么结构？" || !ev[0].Untrusted || ev[0].From != "host" {
		t.Fatalf("guest message: %+v %v", ev, err)
	}
	// Acknowledged events are not delivered twice.
	if ev, _ := guest.recv(ctx, id, 0); types(ev) != "no_events" {
		t.Fatalf("redelivered: %v", types(ev))
	}

	// A blocked recv wakes as soon as the peer sends.
	quiet := guest.o
	quiet.Emit = nil
	sent := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		sent <- ChatSend(ctx, id, ChatMessage{Text: "[{id, name}]"}, quiet)
	}()
	start := time.Now()
	ev, err = host.recv(ctx, id, 30*time.Second)
	if err != nil || types(ev) != "message" || ev[0].Text != "[{id, name}]" {
		t.Fatalf("host long-poll: %+v %v", ev, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("long-poll did not wake promptly: %v", time.Since(start))
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}

	if err := ChatClose(ctx, id, guest.o); err != nil {
		t.Fatal(err)
	}
	if ev, _ := host.recv(ctx, id, time.Second); types(ev) != "closed" || ev[0].By != "peer" {
		t.Fatalf("host sees close: %+v", ev)
	}
	if _, err := host.recv(ctx, id, 0); !errors.Is(err, ErrChatEnded) {
		t.Fatalf("recv after end: %v", err)
	}
	if err := ChatSend(ctx, id, ChatMessage{Text: "late"}, host.o); !errors.Is(err, ErrChatEnded) {
		t.Fatalf("send after end: %v", err)
	}
	transcript, err := os.ReadFile(host.o.StateDir + "/" + id + ".transcript.jsonl")
	if err != nil || strings.Count(string(transcript), "\n") != 6 || !strings.Contains(string(transcript), "[{id, name}]") ||
		!strings.HasPrefix(string(transcript), `{"time":`) || !strings.Contains(strings.SplitN(string(transcript), "\n", 2)[0], `"kind":"started","text":"双方对 /users 响应字段达成一致"`) {
		t.Fatalf("transcript: %q %v", transcript, err)
	}
	if info, err := os.Stat(host.o.StateDir + "/" + id + ".json"); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions: %v %v", info, err)
	}
}

func TestChatDecline(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx := context.Background()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	id, rawCode := invite(t, ctx, host, guest)
	if err := ChatDecline(ctx, rawCode, "今天没空，明天再说", guest.o); err != nil {
		t.Fatal(err)
	}
	ev, err := host.recv(ctx, id, time.Second)
	if err != nil || types(ev) != "opened,declined" || ev[1].Text != "今天没空，明天再说" || !ev[1].Untrusted {
		t.Fatalf("host sees decline: %+v %v", ev, err)
	}
	if _, err := ChatJoin(ctx, rawCode, guest.o); !errors.Is(err, ErrChatEnded) {
		t.Fatalf("join after decline: %v", err)
	}
}

func TestChatExpiresUnclaimed(t *testing.T) {
	cfg := relay.DefaultConfig()
	cfg.ChatInviteTTL = 200 * time.Millisecond
	_, srv := server(t, cfg)
	ctx := context.Background()
	host := newSide(t, srv.URL)
	if err := Send(ctx, fixture(t, []byte("x")), withChat(host.o)); err != nil {
		t.Fatal(err)
	}
	ev, err := host.recv(ctx, host.events[0].Chat, 3*time.Second)
	if err != nil || types(ev) != "expired" || ev[0].Reason != "unclaimed" {
		t.Fatalf("expiry: %+v %v", ev, err)
	}
}

func TestChatNeedsCapableRelay(t *testing.T) {
	uploads := 0
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/handoffs/") {
			uploads++
		}
		http.NotFound(w, r)
	}))
	defer old.Close()
	host := newSide(t, old.URL)
	err := Send(context.Background(), fixture(t, []byte("x")), withChat(host.o))
	if err == nil || !strings.Contains(err.Error(), "does not support chat") || uploads != 0 {
		t.Fatalf("old relay: %v uploads=%d", err, uploads)
	}
}

func TestChatRejectsForgedReplayedAndTampered(t *testing.T) {
	c, _ := code.New()
	key := c.Derive("chat/key")
	room := chatRoomID(c)
	s := &chatState{ID: room, Role: "guest"}
	dir := t.TempDir()
	good, _ := sealChat(key, room, "host", "message", 5, "hi")
	raw, _ := base64.StdEncoding.DecodeString(good)
	raw[len(raw)/2] ^= 1
	tampered := base64.StdEncoding.EncodeToString(raw)
	cur := &chatCursor{}
	if e := s.translate(dir, key, cur, relayEvent{Seq: 1, Type: "message", From: "host", Ctr: 5, Data: good}); e.Event != "message" || e.Text != "hi" {
		t.Fatalf("valid: %+v", e)
	}
	cases := map[string]relayEvent{
		"replayed counter": {Seq: 2, Type: "message", From: "host", Ctr: 5, Data: good},
		"own direction":    {Seq: 3, Type: "message", From: "guest", Ctr: 6, Data: good},
		"relabelled ctr":   {Seq: 4, Type: "message", From: "host", Ctr: 9, Data: good},
		"tampered":         {Seq: 5, Type: "message", From: "host", Ctr: 7, Data: tampered},
		"host-only event":  {Seq: 6, Type: "joined"},
	}
	for name, ev := range cases {
		if e := s.translate(dir, key, cur, ev); e.Event != "error" {
			t.Errorf("%s accepted: %+v", name, e)
		}
	}
	if cur.PeerCtr != 5 {
		t.Fatalf("rejected events moved the counter: %d", cur.PeerCtr)
	}
}

func TestChatJoinRefusesOwnRoom(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx := context.Background()
	host := newSide(t, srv.URL)
	if err := Send(ctx, fixture(t, []byte("x")), withChat(host.o)); err != nil {
		t.Fatal(err)
	}
	if _, err := ChatJoin(ctx, host.events[0].Code, host.o); err == nil || !strings.Contains(err.Error(), "JAND_HOME") {
		t.Fatalf("join into own state dir: %v", err)
	}
}

func TestChatNeedsGoal(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	host := newSide(t, srv.URL)
	o := host.o
	o.Chat = true
	if err := Send(context.Background(), fixture(t, []byte("x")), o); err == nil || !strings.Contains(err.Error(), "goal") {
		t.Fatalf("chat without goal: %v", err)
	}
	o.Goal = strings.Repeat("目", MaxChatGoal)
	if err := Send(context.Background(), fixture(t, []byte("x")), o); err == nil || !strings.Contains(err.Error(), "goal") {
		t.Fatalf("oversized goal: %v", err)
	}
}

func TestChatJoinRetryAndChatIDMistake(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx := context.Background()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	id, rawCode := invite(t, ctx, host, guest)
	if _, err := ChatJoin(ctx, id, guest.o); err == nil || !strings.Contains(err.Error(), "not the chat id") {
		t.Fatalf("join with chat id: %v", err)
	}
	// A gateway that forwards the join but loses the answer.
	target, _ := url.Parse(srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	lossy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/join") {
			proxy.ServeHTTP(httptest.NewRecorder(), r)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer lossy.Close()
	o := guest.o
	o.RelayURL = lossy.URL
	if _, err := ChatJoin(ctx, rawCode, o); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("lost join response: %v", err)
	}
	if _, err := os.Stat(guest.o.StateDir + "/" + id + ".json"); err != nil {
		t.Fatalf("a 5xx must keep the seat token: %v", err)
	}
	if _, err := ChatJoin(ctx, rawCode, guest.o); err != nil {
		t.Fatalf("retried join: %v", err)
	}
	if err := ChatSend(ctx, id, ChatMessage{Text: "在吗"}, guest.o); err != nil {
		t.Fatal(err)
	}
	if ev, _ := host.recv(ctx, id, 0); types(ev) != "opened,joined,message" {
		t.Fatalf("host after retried join: %v", types(ev))
	}
}

func TestChatCheckpointAndNewGoal(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx := context.Background()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	o := withChat(host.o)
	o.Budget = 2
	if err := Send(ctx, fixture(t, []byte("x")), o); err != nil {
		t.Fatal(err)
	}
	id, rawCode := host.events[0].Chat, host.events[0].Code
	if _, err := Receive(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	if saved := guest.events[len(guest.events)-1]; saved.Budget != 2 {
		t.Fatalf("budget not shown to receiver: %+v", saved)
	}
	if _, err := ChatJoin(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	host.recv(ctx, id, 0)

	// The agent reports the goal reached: both sides pause.
	if err := ChatCheckpoint(ctx, id, "字段已对齐：id, name", host.o); err != nil {
		t.Fatal(err)
	}
	ev, _ := guest.recv(ctx, id, time.Second)
	if types(ev) != "checkpoint" || ev[0].By != "peer" || ev[0].Text != "字段已对齐：id, name" || !ev[0].Untrusted || ev[0].Message != PausedNote {
		t.Fatalf("guest sees checkpoint: %+v", ev)
	}
	if err := ChatSend(ctx, id, ChatMessage{Kind: "request", Text: "继续"}, guest.o); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("request while paused: %v", err)
	}
	if err := ChatAccept(ctx, id, host.o); err == nil {
		t.Fatal("accepted with no proposal")
	}
	if err := ChatPropose(ctx, id, "再对 /orders", 2, guest.o); err != nil {
		t.Fatal(err)
	}
	ev, _ = host.recv(ctx, id, time.Second)
	if types(ev) != "proposal" || ev[0].Goal != "再对 /orders" || ev[0].Budget != 2 || !ev[0].Untrusted {
		t.Fatalf("host sees proposal: %+v", ev)
	}
	if err := ChatAccept(ctx, id, host.o); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*side{host, guest} {
		ev, _ := s.recv(ctx, id, time.Second)
		if types(ev) != "resumed" || ev[0].Goal != "再对 /orders" || ev[0].Budget != 2 {
			t.Fatalf("resumed: %+v", ev)
		}
	}
	if ev, _ := guest.recv(ctx, id, 0); types(ev) != "no_events" {
		t.Fatalf("accepted proposal still pending: %v", types(ev))
	}

	// The new budget runs out: the relay pauses without asking the agents.
	for _, text := range []string{"一", "二"} {
		if err := ChatSend(ctx, id, ChatMessage{Text: text}, host.o); err != nil {
			t.Fatal(err)
		}
	}
	ev, _ = guest.recv(ctx, id, time.Second)
	if types(ev) != "message,message,checkpoint" || ev[2].By != "relay" || ev[2].Reason != "budget" {
		t.Fatalf("budget checkpoint: %+v", ev)
	}
	if ev, _ := host.recv(ctx, id, 0); types(ev) != "checkpoint" || ev[0].By != "relay" || ev[0].Message != PausedNote {
		t.Fatalf("host sees budget checkpoint: %+v", ev)
	}
	if err := ChatSend(ctx, id, ChatMessage{Kind: "request", Text: "三"}, host.o); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("message over budget: %v", err)
	}
}

func activePair(t *testing.T) (context.Context, *side, *side, string) {
	t.Helper()
	_, srv := server(t, relay.DefaultConfig())
	ctx := context.Background()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	id, rawCode := invite(t, ctx, host, guest)
	if _, err := ChatJoin(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	host.recv(ctx, id, 0)
	return ctx, host, guest, id
}

func TestChatMessageKindsAndReplies(t *testing.T) {
	ctx, host, guest, id := activePair(t)
	host.events = nil
	if err := ChatSend(ctx, id, ChatMessage{Kind: "request", Text: "字段定义？"}, host.o); err != nil {
		t.Fatal(err)
	}
	if sent := host.events[0]; sent.ID != "h1" || sent.Kind != "request" {
		t.Fatalf("sent: %+v", sent)
	}
	ev, _ := guest.recv(ctx, id, time.Second)
	if ev[0].ID != "h1" || ev[0].Kind != "request" || ev[0].Text != "字段定义？" {
		t.Fatalf("request: %+v", ev)
	}
	for _, bad := range []ChatMessage{
		{Kind: "reply", Text: "x"},                // reply needs a target
		{Kind: "reply", ReplyTo: "g1", Text: "x"}, // must name the peer's message
		{Kind: "reply", ReplyTo: "h01", Text: "x"},
		{Kind: "shout", Text: "x"},
	} {
		if err := ChatSend(ctx, id, bad, guest.o); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := ChatSend(ctx, id, ChatMessage{Kind: "reply", ReplyTo: "h1", Text: "{id:int}"}, guest.o); err != nil {
		t.Fatal(err)
	}
	ev, _ = host.recv(ctx, id, time.Second)
	// Rejected sends fail validation before sealing, so they use no counter.
	if ev[0].Kind != "reply" || ev[0].ReplyTo != "h1" || ev[0].ID != "g1" {
		t.Fatalf("reply: %+v", ev)
	}
}

func TestChatWakeHoldsProgress(t *testing.T) {
	ctx, host, guest, id := activePair(t)
	for _, m := range []ChatMessage{{Kind: "progress", Text: "一"}, {Kind: "progress", Text: "二"}} {
		if err := ChatSend(ctx, id, m, guest.o); err != nil {
			t.Fatal(err)
		}
	}
	quiet := guest.o
	quiet.Emit = nil
	sent := make(chan error, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		sent <- ChatSend(ctx, id, ChatMessage{Kind: "request", Text: "三"}, quiet)
	}()
	host.events = nil
	start := time.Now()
	if err := ChatRecv(ctx, id, 20*time.Second, []string{"request", "reply", "delivery"}, host.o); err != nil {
		t.Fatal(err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	// Progress did not end the wait; the request did, and all three arrive in order.
	if took := time.Since(start); took < time.Second || types(host.events) != "message,message,message" ||
		host.events[0].Text != "一" || host.events[2].Kind != "request" {
		t.Fatalf("after %v: %+v", took, host.events)
	}
	// Held progress is released when a wait runs out, too.
	ChatSend(ctx, id, ChatMessage{Kind: "progress", Text: "四"}, guest.o)
	host.events = nil
	if err := ChatRecv(ctx, id, 1500*time.Millisecond, []string{"request"}, host.o); err != nil || types(host.events) != "message" || host.events[0].Text != "四" {
		t.Fatalf("timeout release: %+v %v", host.events, err)
	}
}

func TestChatDoneBothSides(t *testing.T) {
	ctx, host, guest, id := activePair(t)
	if err := ChatDone(ctx, id, "后端完成", guest.o); err != nil {
		t.Fatal(err)
	}
	if err := ChatDone(ctx, id, "", guest.o); err == nil {
		t.Fatal("reported done twice")
	}
	ev, _ := host.recv(ctx, id, time.Second)
	if types(ev) != "done" || ev[0].By != "peer" || ev[0].Text != "后端完成" {
		t.Fatalf("host sees peer done: %+v", ev)
	}
	// One side done is not a pause: work and requests continue.
	if err := ChatSend(ctx, id, ChatMessage{Kind: "request", Text: "再加个字段"}, host.o); err != nil {
		t.Fatal(err)
	}
	if err := ChatDone(ctx, id, "前端完成", host.o); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*side{host, guest} {
		ev, _ := s.recv(ctx, id, time.Second)
		last := ev[len(ev)-1]
		if last.Event != "checkpoint" || last.By != "relay" || last.Reason != "all_done" {
			t.Fatalf("all done: %+v", ev)
		}
	}
	if err := ChatPropose(ctx, id, "修 bug", 5, guest.o); err != nil {
		t.Fatal(err)
	}
	host.recv(ctx, id, time.Second)
	if err := ChatAccept(ctx, id, host.o); err != nil {
		t.Fatal(err)
	}
	host.recv(ctx, id, time.Second)
	// A new goal starts with nobody done.
	if err := ChatDone(ctx, id, "", host.o); err != nil {
		t.Fatalf("done after resume: %v", err)
	}
}

func TestChatUnreadBlocksAndSupersedes(t *testing.T) {
	ctx, host, guest, id := activePair(t)
	send := func(sd *side, m ChatMessage) error { return ChatSend(ctx, id, m, sd.o) }
	if err := send(host, ChatMessage{Kind: "delivery", Text: "草稿 v1"}); err != nil { // h1
		t.Fatal(err)
	}
	// The guest has not read h1: a reply now would answer blind.
	err := send(guest, ChatMessage{Kind: "request", Text: "字段？"})
	if !errors.Is(err, ErrUnread) || !strings.Contains(err.Error(), "h1") {
		t.Fatalf("unread not caught: %v", err)
	}
	// Progress and notes are never held back, and never block the peer:
	// the host can still deliver with g1 (progress) unread.
	if err := send(guest, ChatMessage{Kind: "progress", Text: "在写"}); err != nil {
		t.Fatal(err)
	}
	guest.recv(ctx, id, 0)
	// The host replaces h1 before the guest's review arrives.
	if err := send(host, ChatMessage{Kind: "delivery", Supersedes: "h1", Text: "草稿 v2"}); err != nil { // h2
		t.Fatal(err)
	}
	if err := send(host, ChatMessage{Kind: "delivery", Supersedes: "g1", Text: "x"}); err == nil {
		t.Fatal("superseded the peer's message")
	}
	// Written against h1 without reading h2: --anyway, as a crossing would.
	if err := send(guest, ChatMessage{Kind: "reply", ReplyTo: "h1", Text: "v1 有问题", Anyway: true}); err != nil {
		t.Fatal(err)
	}
	ev, _ := host.recv(ctx, id, time.Second)
	last := ev[len(ev)-1]
	if !last.Stale || last.SupersededBy != "h2" || last.ReplyTo != "h1" {
		t.Fatalf("stale reply not flagged: %+v", ev)
	}
	ev, _ = guest.recv(ctx, id, time.Second)
	if ev[0].Supersedes != "h1" {
		t.Fatalf("supersedes not shown: %+v", ev)
	}
	// Having read h2, the guest may not reply to h1 without --anyway.
	if err := send(guest, ChatMessage{Kind: "reply", ReplyTo: "h1", Text: "再说 v1"}); err == nil || !strings.Contains(err.Error(), "superseded by h2") {
		t.Fatalf("reply to superseded: %v", err)
	}
	if err := send(guest, ChatMessage{Kind: "reply", ReplyTo: "h2", Text: "v2 可以"}); err != nil {
		t.Fatal(err)
	}
}

func TestChatClosingNotesWhilePaused(t *testing.T) {
	ctx, host, guest, id := activePair(t)
	ChatDone(ctx, id, "", host.o)
	ChatDone(ctx, id, "", guest.o)
	host.recv(ctx, id, time.Second)
	guest.recv(ctx, id, time.Second)
	for i := range 3 {
		if err := ChatSend(ctx, id, ChatMessage{Kind: "note", Text: "收尾"}, host.o); err != nil {
			t.Fatalf("closing note %d: %v", i, err)
		}
	}
	if err := ChatSend(ctx, id, ChatMessage{Kind: "note", Text: "第四条"}, host.o); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("fourth closing note: %v", err)
	}
	if err := ChatSend(ctx, id, ChatMessage{Kind: "delivery", Text: "新东西"}, guest.o); err == nil {
		t.Fatal("delivery passed a pause")
	}
	ev, _ := guest.recv(ctx, id, time.Second)
	if types(ev) != "message,message,message" || !ev[0].AfterPause {
		t.Fatalf("closing notes: %+v", ev)
	}
	// The guest still has its own three, even as a reply.
	if err := ChatSend(ctx, id, ChatMessage{Kind: "reply", ReplyTo: "h1", Text: "收到"}, guest.o); err != nil {
		t.Fatal(err)
	}
}

func reviewWorkflow() *Workflow {
	one := 1
	return &Workflow{Name: "review", DoneRule: "any", PauseNotes: &one, Budget: 12,
		Kinds: []string{"request", "reply", "delivery"}, Roles: map[string]string{"host": "审核", "guest": "实现"},
		Instructions: "实现方交付，审核方回复意见。"}
}

func TestChatWorkflowEndToEnd(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx := context.Background()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	o := withChat(host.o)
	o.Workflow = reviewWorkflow()
	if err := Send(ctx, fixture(t, []byte("x")), o); err != nil {
		t.Fatal(err)
	}
	id, rawCode := host.events[0].Chat, host.events[0].Code
	if _, err := Receive(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	saved := guest.events[len(guest.events)-1]
	if saved.Message != "" || saved.Workflow == nil || saved.Workflow.Name != "review" || saved.Workflow.DoneRule != "any" ||
		saved.Budget != 12 || saved.Workflow.Roles["guest"] != "实现" {
		t.Fatalf("saved: %+v %+v", saved, saved.Workflow)
	}
	if _, err := ChatJoin(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	host.recv(ctx, id, 0)
	// The workflow leaves out notes and progress.
	if err := ChatSend(ctx, id, ChatMessage{Text: "闲聊"}, guest.o); err == nil || !strings.Contains(err.Error(), "does not allow note") {
		t.Fatalf("note under review workflow: %v", err)
	}
	if err := ChatSend(ctx, id, ChatMessage{Kind: "delivery", Text: "实现完成"}, guest.o); err != nil {
		t.Fatal(err)
	}
	// done_rule=any: the implementer's done pauses the chat for acceptance.
	if err := ChatDone(ctx, id, "请验收", guest.o); err != nil {
		t.Fatal(err)
	}
	ev, _ := host.recv(ctx, id, time.Second)
	if last := ev[len(ev)-1]; last.Event != "checkpoint" || last.Reason != "any_done" {
		t.Fatalf("any_done: %+v", ev)
	}
	// One closing reply each, as the workflow says.
	if err := ChatSend(ctx, id, ChatMessage{Kind: "reply", ReplyTo: "g1", Text: "收到"}, host.o); err != nil {
		t.Fatal(err)
	}
	if err := ChatSend(ctx, id, ChatMessage{Kind: "reply", ReplyTo: "g1", Text: "再补一句", Anyway: true}, host.o); err == nil {
		t.Fatal("second closing note allowed with pause_notes=1")
	}
}

// tamper rewrites the relay's charter answer, as a dishonest relay could.
func tamper(t *testing.T, target string, edit func(map[string]any)) *httptest.Server {
	u, _ := url.Parse(target)
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.ModifyResponse = func(r *http.Response) error {
		if !strings.HasSuffix(r.Request.URL.Path, "/charter") || r.StatusCode != 200 {
			return nil
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		edit(body)
		data, _ := json.Marshal(body)
		r.Body = io.NopCloser(bytes.NewReader(data))
		r.ContentLength = int64(len(data))
		r.Header.Set("Content-Length", strconv.Itoa(len(data)))
		return nil
	}
	s := httptest.NewServer(proxy)
	t.Cleanup(s.Close)
	return s
}

func TestChatCharterTamperingIsCaught(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx := context.Background()
	host := newSide(t, srv.URL)
	o := withChat(host.o)
	o.Workflow = reviewWorkflow()
	if err := Send(ctx, fixture(t, []byte("x")), o); err != nil {
		t.Fatal(err)
	}
	rawCode := host.events[0].Code
	// The relay claims a looser rule than the host sealed.
	lying := tamper(t, srv.URL, func(b map[string]any) { b["done_rule"] = "all" })
	guest := newSide(t, lying.URL)
	if _, err := Receive(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	saved := guest.events[len(guest.events)-1]
	if saved.Workflow != nil || !strings.Contains(saved.Message, "do not join") {
		t.Fatalf("tampered terms accepted: %+v", saved)
	}
	if _, err := ChatJoin(ctx, rawCode, guest.o); err == nil || !strings.Contains(err.Error(), "not joining") {
		t.Fatalf("joined despite tampering: %v", err)
	}
}

func TestChatWorkflowNeedsCapableRelay(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	// A 0.4.0 relay: everything but the charter endpoint.
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/charter") {
			http.NotFound(w, r)
			return
		}
		u, _ := url.Parse(srv.URL)
		httputil.NewSingleHostReverseProxy(u).ServeHTTP(w, r)
	}))
	defer old.Close()
	ctx := context.Background()
	host := newSide(t, old.URL)
	o := withChat(host.o)
	o.Workflow = reviewWorkflow()
	if err := Send(ctx, fixture(t, []byte("x")), o); err == nil || !strings.Contains(err.Error(), "0.4.1") {
		t.Fatalf("workflow on an old relay: %v", err)
	}
	// The default workflow still works there, exactly as 0.4.0 did.
	host.events = nil
	if err := Send(ctx, fixture(t, []byte("x")), withChat(host.o)); err != nil {
		t.Fatal(err)
	}
	guest := newSide(t, old.URL)
	if _, err := Receive(ctx, host.events[0].Code, guest.o); err != nil {
		t.Fatal(err)
	}
	if saved := guest.events[len(guest.events)-1]; saved.Message != "" || saved.Workflow == nil || saved.Workflow.Name != "default" {
		t.Fatalf("default workflow on old relay: %+v", saved)
	}
}

func TestWorkflowValidate(t *testing.T) {
	good := DefaultWorkflow()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	eleven := 11
	for name, w := range map[string]Workflow{
		"no name":           {DoneRule: "all"},
		"rule":              {Name: "x", DoneRule: "some"},
		"unknown kind":      {Name: "x", Kinds: []string{"shout"}},
		"request, no reply": {Name: "x", Kinds: []string{"request", "delivery"}},
		"pause ceiling":     {Name: "x", PauseNotes: &eleven},
		"third role":        {Name: "x", Roles: map[string]string{"observer": "x"}},
		"path in name":      {Name: "../x"},
	} {
		if err := w.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRepositoryWorkflowsAreValid(t *testing.T) {
	for _, name := range []string{"pair-dev", "review"} {
		w, err := LoadWorkflow("../../workflows/" + name + ".json")
		if err != nil || w.Name != name {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestChatWakeNeverHoldsNotes(t *testing.T) {
	ctx, host, guest, id := activePair(t)
	// An unlabelled message defaults to note and may be a request in disguise.
	if err := ChatSend(ctx, id, ChatMessage{Text: "接口字段是什么？"}, guest.o); err != nil {
		t.Fatal(err)
	}
	host.events = nil
	start := time.Now()
	if err := ChatRecv(ctx, id, 10*time.Second, []string{"request"}, host.o); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second || types(host.events) != "message" || host.events[0].Kind != "note" {
		t.Fatalf("note held back: %+v", host.events)
	}
}

func TestSendWarnsAboutUnfilledTemplate(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	template, err := os.ReadFile("../../docs/jand-template.md")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		content []byte
		warn    bool
	}{
		"blank template": {template, true},
		"filled":         {[]byte("# 交接\n- 目标：对齐接口\n- 来源：仓库 main\n- 状态：进行中\n- 风险：无\n- 下一步：联调\n"), false},
		"prose":          {[]byte("# 交接\n\n请对齐 /users 接口。\n"), false},
	} {
		var events []Event
		o := Options{RelayURL: srv.URL, Emit: func(e Event) { events = append(events, e) }}
		if err := Send(context.Background(), fixture(t, tc.content), o); err != nil {
			t.Fatal(err)
		}
		warned := events[0].Event == "warning"
		if warned != tc.warn || events[len(events)-1].Event != "queued" {
			t.Errorf("%s: %+v", name, events)
		}
	}
}
