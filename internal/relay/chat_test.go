package relay

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func tok(b byte) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(string(rune('a'+b)), 32)))
}

var chatRoom1 = strings.Repeat("ab", 16)

func call(t *testing.T, base, method, path, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// activeChat creates a room and joins it: host token tok(0), guest tok(2).
func activeChat(t *testing.T, cfg Config) (*Relay, string) {
	return activeChatBudget(t, cfg, 0)
}

func activeChatBudget(t *testing.T, cfg Config, budget int) (*Relay, string) {
	r := New(cfg)
	t.Cleanup(r.Close)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	p := "/v1/chats/" + chatRoom1
	if got, _ := call(t, srv.URL, "PUT", p, "", `{"host":"`+tok(0)+`","invite":"`+tok(1)+`","budget":`+itoa(uint64(budget))+`}`); got != 201 {
		t.Fatalf("create: %d", got)
	}
	if got, _ := call(t, srv.URL, "POST", p+"/join", tok(1), `{"guest":"`+tok(2)+`"}`); got != 204 {
		t.Fatalf("join: %d", got)
	}
	return r, srv.URL
}

var cipher30 = base64.StdEncoding.EncodeToString(make([]byte, 30))

func TestChatAuthorization(t *testing.T) {
	_, base := activeChat(t, DefaultConfig())
	p := "/v1/chats/" + chatRoom1
	msg := `{"ctr":1,"data":"` + cipher30 + `"}`
	if got, _ := call(t, base, "POST", p+"/messages", tok(1), msg); got != 404 {
		t.Fatalf("invite token may not send after join: %d", got)
	}
	if got, _ := call(t, base, "POST", p+"/join", tok(1), `{"guest":"`+tok(3)+`"}`); got != 409 {
		t.Fatalf("seat is locked: %d", got)
	}
	if got, _ := call(t, base, "POST", p+"/join", tok(1), `{"guest":"`+tok(2)+`"}`); got != 204 {
		t.Fatalf("retried join with the seat's own token: %d", got)
	}
	if got, _ := call(t, base, "POST", p+"/join", tok(3), `{"guest":"`+tok(2)+`"}`); got != 404 {
		t.Fatalf("retry without the invite token: %d", got)
	}
	if got, _ := call(t, base, "GET", p+"/events", tok(3), ""); got != 404 {
		t.Fatalf("stranger reads events: %d", got)
	}
	if got, _ := call(t, base, "POST", p+"/messages", tok(2), `{"ctr":0,"data":"`+cipher30+`"}`); got != 400 {
		t.Fatalf("zero counter: %d", got)
	}
	if got, _ := call(t, base, "POST", p+"/messages", tok(2), msg); got != 201 {
		t.Fatalf("guest sends: %d", got)
	}
	_, body := call(t, base, "GET", p+"/events?after=0", tok(0), "")
	if !strings.Contains(body, `"type":"joined"`) || !strings.Contains(body, `"from":"guest"`) {
		t.Fatalf("host events: %s", body)
	}
	_, body = call(t, base, "GET", p+"/events", tok(2), "")
	if strings.Contains(body, `"type":"message"`) {
		t.Fatalf("sender received its own message: %s", body)
	}
}

func TestChatLongPollWakeAndWaiterLimit(t *testing.T) {
	_, base := activeChat(t, DefaultConfig())
	p := "/v1/chats/" + chatRoom1
	// Drain the joined event first.
	call(t, base, "GET", p+"/events?after=1", tok(0), "")
	var wg sync.WaitGroup
	results := make([]int, maxChatWaiters+1)
	for i := range maxChatWaiters {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = call(t, base, "GET", p+"/events?after=1&wait=10", tok(0), "")
		}(i)
	}
	time.Sleep(200 * time.Millisecond)
	if got, _ := call(t, base, "GET", p+"/events?after=1&wait=10", tok(0), ""); got != 429 {
		t.Fatalf("extra waiter: %d", got)
	}
	start := time.Now()
	call(t, base, "POST", p+"/messages", tok(2), `{"ctr":1,"data":"`+cipher30+`"}`)
	wg.Wait()
	if time.Since(start) > 2*time.Second {
		t.Fatal("waiters were not woken")
	}
	for i := range maxChatWaiters {
		if results[i] != 200 {
			t.Fatalf("waiter %d: %d", i, results[i])
		}
	}
}

type page struct {
	State  string
	Events []chatEvent
}

func events(t *testing.T, base, path, token string) page {
	t.Helper()
	_, body := call(t, base, "GET", path, token, "")
	var p page
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("events %q: %v", body, err)
	}
	return p
}

func msg(ctr int) string {
	return `{"ctr":` + itoa(uint64(ctr)) + `,"data":"` + cipher30 + `"}`
}

func TestChatCheckpointProposeAccept(t *testing.T) {
	_, base := activeChat(t, DefaultConfig())
	p := "/v1/chats/" + chatRoom1
	if got, _ := call(t, base, "POST", p+"/propose", tok(0), msg(1)); got != 409 {
		t.Fatalf("propose while active: %d", got)
	}
	if got, _ := call(t, base, "POST", p+"/checkpoint", tok(0), msg(1)); got != 204 {
		t.Fatalf("checkpoint: %d", got)
	}
	if got, _ := call(t, base, "POST", p+"/checkpoint", tok(2), ""); got != 409 {
		t.Fatalf("second checkpoint: %d", got)
	}
	if got, _ := call(t, base, "POST", p+"/messages", tok(2), msg(1)); got != 409 {
		t.Fatalf("message while paused: %d", got)
	}
	g := events(t, base, p+"/events", tok(2))
	if g.State != "paused" || len(g.Events) != 1 || g.Events[0].Type != "checkpoint" || g.Events[0].Reason != "goal_reached" || g.Events[0].Data == "" {
		t.Fatalf("guest sees checkpoint: %+v", g)
	}
	if got, _ := call(t, base, "POST", p+"/propose", tok(2), `{"ctr":2,"data":"`+cipher30+`","budget":7}`); got != 204 {
		t.Fatalf("guest proposes: %d", got)
	}
	h := events(t, base, p+"/events?after=1", tok(0))
	prop := h.Events[len(h.Events)-1]
	if prop.Type != "proposal" || prop.Budget != 7 || prop.From != "guest" {
		t.Fatalf("host sees proposal: %+v", h)
	}
	if got, _ := call(t, base, "POST", p+"/accept", tok(2), `{"seq":`+itoa(prop.Seq)+`}`); got != 409 {
		t.Fatalf("proposer accepts own proposal: %d", got)
	}
	if got, _ := call(t, base, "POST", p+"/accept", tok(0), `{"seq":`+itoa(prop.Seq+100)+`}`); got != 409 {
		t.Fatalf("accept a stale sequence: %d", got)
	}
	if got, _ := call(t, base, "POST", p+"/accept", tok(0), `{"seq":`+itoa(prop.Seq)+`}`); got != 204 {
		t.Fatalf("host accepts: %d", got)
	}
	for _, token := range []string{tok(0), tok(2)} {
		e := events(t, base, p+"/events?after="+itoa(prop.Seq), token)
		last := e.Events[len(e.Events)-1]
		if e.State != "active" || last.Type != "resumed" || last.Budget != 7 || last.From != "guest" || last.Data == "" {
			t.Fatalf("resumed: %+v", e)
		}
	}
	if got, _ := call(t, base, "POST", p+"/messages", tok(2), msg(3)); got != 201 {
		t.Fatalf("message after resume: %d", got)
	}
}

func TestChatBudgetAndLimitAndAccounting(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ChatMaxMessages = 3
	cfg.ChatEndedTTL = 100 * time.Millisecond
	r, base := activeChatBudget(t, cfg, 2)
	p := "/v1/chats/" + chatRoom1
	for i := 1; i <= 2; i++ {
		if got, _ := call(t, base, "POST", p+"/messages", tok(0), msg(i)); got != 201 {
			t.Fatalf("message %d: %d", i, got)
		}
	}
	// The budget is spent: the relay pauses the chat for both sides.
	if got, _ := call(t, base, "POST", p+"/messages", tok(0), msg(3)); got != 409 {
		t.Fatalf("over budget: %d", got)
	}
	g := events(t, base, p+"/events", tok(2))
	last := g.Events[len(g.Events)-1]
	if g.State != "paused" || last.Type != "checkpoint" || last.Reason != "budget" || last.From != "" {
		t.Fatalf("budget checkpoint: %+v", g)
	}
	call(t, base, "POST", p+"/propose", tok(0), `{"ctr":3,"data":"`+cipher30+`","budget":3}`)
	g = events(t, base, p+"/events?after="+itoa(last.Seq), tok(2))
	call(t, base, "POST", p+"/accept", tok(2), `{"seq":`+itoa(g.Events[0].Seq)+`}`)
	if got, _ := call(t, base, "POST", p+"/messages", tok(0), msg(4)); got != 201 {
		t.Fatalf("message after resume: %d", got)
	}
	// Three messages in total: the whole-chat limit ends it whatever the budget.
	if got, _ := call(t, base, "POST", p+"/messages", tok(0), msg(5)); got != 410 {
		t.Fatalf("over the chat limit: %d", got)
	}
	g = events(t, base, p+"/events", tok(2))
	end := g.Events[len(g.Events)-1]
	if g.State != "ended" || end.Type != "expired" || end.Reason != "limit" {
		t.Fatalf("limit: %+v", g)
	}
	h := events(t, base, p+"/events", tok(0))
	call(t, base, "GET", p+"/events?after="+itoa(end.Seq), tok(2), "")
	call(t, base, "GET", p+"/events?after="+itoa(h.Events[len(h.Events)-1].Seq), tok(0), "")
	r.mu.Lock()
	queued := r.chatBytes
	r.mu.Unlock()
	if queued != 0 {
		t.Fatalf("acknowledged ciphertext still counted: %d", queued)
	}
	time.Sleep(150 * time.Millisecond)
	if r.ActiveChats() != 0 {
		t.Fatal("ended chat still active")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.chatBytes != 0 || len(r.chats) != 0 {
		t.Fatalf("leaked: bytes=%d rooms=%d", r.chatBytes, len(r.chats))
	}
}

func TestChatIdleExpiry(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ChatIdleTTL = 150 * time.Millisecond
	_, base := activeChat(t, cfg)
	p := "/v1/chats/" + chatRoom1
	_, body := call(t, base, "GET", p+"/events?after=0&wait=2", tok(2), "")
	if !strings.Contains(body, `"reason":"idle"`) {
		t.Fatalf("idle: %s", body)
	}
	if got, _ := call(t, base, "POST", p+"/messages", tok(0), `{"ctr":1,"data":"`+cipher30+`"}`); got != 410 {
		t.Fatalf("send after idle: %d", got)
	}
}

func itoa(n uint64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func createRoom(t *testing.T, base, extra string) {
	t.Helper()
	body := `{"host":"` + tok(0) + `","invite":"` + tok(1) + `"` + extra + `}`
	if got, reason := call(t, base, "PUT", "/v1/chats/"+chatRoom1, "", body); got != 201 {
		t.Fatalf("create %s: %d %s", extra, got, reason)
	}
}

func TestChatWorkflowTerms(t *testing.T) {
	r := New(DefaultConfig())
	t.Cleanup(r.Close)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	p := "/v1/chats/" + chatRoom1
	if got, reason := call(t, srv.URL, "PUT", p, "", `{"host":"`+tok(0)+`","invite":"`+tok(1)+`","pause_notes":4}`); got != 400 || !strings.Contains(reason, "limit of 3") {
		t.Fatalf("pause_notes over the relay limit: %d %s", got, reason)
	}
	createRoom(t, srv.URL, `,"done_rule":"any","pause_notes":0,"charter":"`+cipher30+`"`)
	// The guest reads the terms with the invite token before joining.
	_, terms := call(t, srv.URL, "GET", p+"/charter", tok(1), "")
	if !strings.Contains(terms, `"done_rule":"any"`) || !strings.Contains(terms, `"pause_notes":0`) || !strings.Contains(terms, cipher30) {
		t.Fatalf("terms: %s", terms)
	}
	if got, _ := call(t, srv.URL, "GET", p+"/charter", tok(3), ""); got != 404 {
		t.Fatalf("stranger reads the charter: %d", got)
	}
	// An older client would misread an any-rule pause, so it may not join.
	if got, reason := call(t, srv.URL, "POST", p+"/join", tok(1), `{"guest":"`+tok(2)+`"}`); got != 409 || !strings.Contains(reason, "0.4.1") {
		t.Fatalf("old client joins an any-rule chat: %d %s", got, reason)
	}
	if got, _ := call(t, srv.URL, "POST", p+"/join", tok(1), `{"guest":"`+tok(2)+`","features":["charter"]}`); got != 204 {
		t.Fatalf("join: %d", got)
	}
	// Under done_rule=any the first done pauses the chat.
	if got, _ := call(t, srv.URL, "POST", p+"/done", tok(2), ""); got != 204 {
		t.Fatalf("done: %d", got)
	}
	h := events(t, srv.URL, p+"/events", tok(0))
	last := h.Events[len(h.Events)-1]
	if h.State != "paused" || last.Type != "checkpoint" || last.Reason != "any_done" {
		t.Fatalf("any_done: %+v", h)
	}
	// pause_notes=0: no closing messages for this chat.
	if got, _ := call(t, srv.URL, "POST", p+"/messages", tok(0), `{"ctr":1,"data":"`+cipher30+`","closing":true}`); got != 409 {
		t.Fatalf("closing note with pause_notes=0: %d", got)
	}
}
