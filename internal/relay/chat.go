package relay

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jaredchao/jand/internal/code"
)

// Chat rooms carry end-to-end encrypted messages between two parties that
// each poll for their own events. The relay sees derived tokens, sizes and
// timing, never the transfer code, the message key, the goal or plaintext.
//
// A chat works toward one goal at a time with a message budget. Each party
// reports its own share done; once both have, or either calls an immediate
// checkpoint, or the budget runs out, the room pauses:
// no messages pass until one side proposes a goal and the other accepts it,
// each after asking its own user. The relay enforces the pause; it cannot
// read the goal, only count.

const (
	// MaxChatCiphertext bounds one encrypted message: 64 KiB of text plus
	// the GCM nonce and tag.
	MaxChatCiphertext = 64*1024 + 28
	maxChatWaiters    = 4
)

const (
	chatPending = "pending" // created; handoff packet not yet claimed
	chatOpened  = "opened"  // packet claimed; guest has not decided
	chatActive  = "active"  // guest joined; messages flow within the budget
	chatPaused  = "paused"  // checkpoint: waiting for an accepted new goal
	chatEnded   = "ended"   // closed, declined or expired; kept briefly so pollers learn why
)

type chatEvent struct {
	Seq    uint64 `json:"seq"`
	Type   string `json:"type"`
	From   string `json:"from,omitempty"`
	Ctr    uint64 `json:"ctr,omitempty"`
	Data   string `json:"data,omitempty"`
	Reason string `json:"reason,omitempty"`
	Budget int    `json:"budget,omitempty"`
	// Paused marks a closing message sent while the chat was paused.
	Paused bool `json:"paused,omitempty"`
}

type chatRoom struct {
	host, invite, guest string
	state               string
	created, changed    time.Time // changed: last state change or message
	seq                 uint64
	queue               map[string][]chatEvent // keyed by recipient role
	bytes               int64
	messages            int             // over the whole chat, against ChatMaxMessages
	budget, used        int             // for the current goal
	done                map[string]bool // parties that reported their share of the goal done
	pauseNotes          map[string]int  // closing messages sent during the current pause
	// Terms set by the host's workflow at creation and enforced here. The
	// charter is the host's sealed copy, which the guest checks them against.
	doneRule      string // "all" or "any"
	pauseNotesMax int
	charter       string
	proposal      *chatEvent
	waiters       int
	notify        chan struct{}
}

func other(role string) string {
	if role == "host" {
		return "guest"
	}
	return "host"
}

// pushLocked queues an event and accounts for its ciphertext.
func (r *Relay) pushLocked(c *chatRoom, to string, e chatEvent) chatEvent {
	c.seq++
	e.Seq = c.seq
	c.queue[to] = append(c.queue[to], e)
	c.bytes += int64(len(e.Data))
	r.chatBytes += int64(len(e.Data))
	return e
}

func (c *chatRoom) wake() {
	close(c.notify)
	c.notify = make(chan struct{})
}

// deadline returns when the room leaves its current state, and why.
func (r *Relay) chatDeadline(c *chatRoom) (time.Time, string) {
	cfg := r.config
	switch c.state {
	case chatPending:
		return c.created.Add(cfg.ChatInviteTTL), "unclaimed"
	case chatOpened, chatPaused:
		return c.changed.Add(cfg.ChatDecideTTL), "no_decision"
	case chatActive:
		idle, life := c.changed.Add(cfg.ChatIdleTTL), c.created.Add(cfg.ChatLifetime)
		if life.Before(idle) {
			return life, "lifetime"
		}
		return idle, "idle"
	default:
		return c.changed.Add(cfg.ChatEndedTTL), ""
	}
}

func (r *Relay) expireChatsLocked() {
	now := time.Now()
	for room, c := range r.chats {
		deadline, reason := r.chatDeadline(c)
		if now.Before(deadline) {
			continue
		}
		if c.state == chatEnded {
			r.chatBytes -= c.bytes
			delete(r.chats, room)
			c.wake()
			continue
		}
		r.endChatLocked(room, c, chatEvent{Type: "expired", Reason: reason}, "host", "guest")
	}
}

// endChatLocked moves a room to its terminal state. Queued messages stay so
// the peer can still read what was sent before the end event.
func (r *Relay) endChatLocked(room string, c *chatRoom, e chatEvent, to ...string) {
	for _, role := range to {
		r.pushLocked(c, role, e)
	}
	r.log.Info("chat ended", "room", tag(room), "event", e.Type, "by", e.From, "reason", e.Reason,
		"from_state", c.state, "messages", c.messages)
	c.state = chatEnded
	c.changed = time.Now()
	c.proposal = nil
	c.wake()
}

func (r *Relay) setStateLocked(c *chatRoom, state string) {
	c.state, c.changed = state, time.Now()
	c.wake()
}

func (r *Relay) ActiveChats() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireChatsLocked()
	n := 0
	for _, c := range r.chats {
		if c.state != chatEnded {
			n++
		}
	}
	return n
}

func (r *Relay) serveChat(w http.ResponseWriter, req *http.Request, path string) {
	room, action, _ := strings.Cut(path, "/")
	if !code.ValidRoom(room) {
		http.NotFound(w, req)
		return
	}
	switch {
	case req.Method == http.MethodPut && action == "":
		r.chatCreate(w, req, room)
	case req.Method == http.MethodGet && action == "events":
		r.chatEvents(w, req, room)
	case req.Method == http.MethodGet && action == "charter":
		r.chatCharter(w, req, room)
	case req.Method == http.MethodPost && (action == "opened" || action == "join" || action == "decline"):
		r.chatInvite(w, req, room, action)
	case req.Method == http.MethodPost && (action == "messages" || action == "checkpoint" || action == "done" || action == "propose" || action == "accept" || action == "close"):
		r.chatParty(w, req, room, action)
	default:
		http.NotFound(w, req)
	}
}

func readJSON(w http.ResponseWriter, req *http.Request, limit int64, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, limit))
	if err != nil {
		return false
	}
	if len(body) == 0 {
		return true
	}
	return json.Unmarshal(body, v) == nil
}

func (r *Relay) validBudget(n int) bool {
	return n >= 1 && n <= r.config.ChatMaxMessages
}

func (r *Relay) chatCreate(w http.ResponseWriter, req *http.Request, room string) {
	var body struct {
		Host, Invite string
		Budget       int
		DoneRule     string `json:"done_rule"`
		PauseNotes   *int   `json:"pause_notes"`
		Charter      string
	}
	if !readJSON(w, req, maxCharter+1024, &body) || !validToken(body.Host) || !validToken(body.Invite) || body.Host == body.Invite {
		http.Error(w, "invalid chat", http.StatusBadRequest)
		return
	}
	if body.Budget == 0 {
		body.Budget = r.config.ChatDefaultBudget
	}
	if !r.validBudget(body.Budget) {
		http.Error(w, fmt.Sprintf("invalid budget; this relay allows 1 to %d", r.config.ChatMaxMessages), http.StatusBadRequest)
		return
	}
	if body.DoneRule == "" {
		body.DoneRule = "all"
	}
	if body.DoneRule != "all" && body.DoneRule != "any" {
		http.Error(w, "invalid done_rule", http.StatusBadRequest)
		return
	}
	pauseNotes := r.config.ChatPauseNotes
	if body.PauseNotes != nil {
		if *body.PauseNotes < 0 || *body.PauseNotes > r.config.ChatPauseNotes {
			http.Error(w, fmt.Sprintf("pause_notes exceeds this relay's limit of %d", r.config.ChatPauseNotes), http.StatusBadRequest)
			return
		}
		pauseNotes = *body.PauseNotes
	}
	if body.Charter != "" && !validCharter(body.Charter) {
		http.Error(w, "invalid charter", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireChatsLocked()
	if r.closed {
		http.Error(w, "relay unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, exists := r.chats[room]; exists {
		http.Error(w, "code collision", http.StatusConflict)
		return
	}
	if len(r.chats) >= r.config.MaxChats {
		r.log.Warn("chat rejected", "room", tag(room), "reason", "relay full", "chats", len(r.chats))
		http.Error(w, "relay full", http.StatusServiceUnavailable)
		return
	}
	now := time.Now()
	c := &chatRoom{host: body.Host, invite: body.Invite, state: chatPending, created: now, changed: now,
		budget: body.Budget, queue: map[string][]chatEvent{}, done: map[string]bool{}, pauseNotes: map[string]int{}, notify: make(chan struct{}),
		doneRule: body.DoneRule, pauseNotesMax: pauseNotes, charter: body.Charter}
	r.chats[room] = c
	c.bytes += int64(len(body.Charter))
	r.chatBytes += int64(len(body.Charter))
	r.log.Info("chat created", "room", tag(room), "budget", body.Budget, "done_rule", body.DoneRule,
		"pause_notes", pauseNotes, "charter", body.Charter != "", "chats", len(r.chats))
	w.WriteHeader(http.StatusCreated)
}

// role authenticates a party token. The invite token is not a role: it can
// only report the claim, join or decline.
func (c *chatRoom) role(token string) string {
	if !validToken(token) {
		return ""
	}
	if equal(token, c.host) {
		return "host"
	}
	if c.guest != "" && equal(token, c.guest) {
		return "guest"
	}
	return ""
}

type chatBody struct {
	Guest string
	// Features the joining client supports; "charter" means it reads the
	// host's workflow. A room whose rules an older client would misread
	// refuses clients without it.
	Features []string
	Ctr      uint64
	Data     string
	Budget   int
	Seq      uint64
	// Closing is set by the client for reply and note messages: the only
	// kinds that may pass a pause. The kind itself is encrypted, so this
	// relies on honest clients; the count limit does not.
	Closing bool
}

// sealed reports whether the body carries a well-formed encrypted payload.
// Empty is allowed only when optional is set.
func (b chatBody) sealed(optional bool) bool {
	if b.Data == "" {
		return optional && b.Ctr == 0
	}
	raw, err := base64.StdEncoding.DecodeString(b.Data)
	return err == nil && b.Ctr > 0 && len(raw) >= 29 && len(raw) <= MaxChatCiphertext
}

func (r *Relay) roomFull(room string, size int) bool {
	if r.chatBytes+int64(size) <= r.config.MaxChatBytes {
		return false
	}
	r.log.Warn("chat request rejected", "room", tag(room), "reason", "relay full", "chat_bytes", r.chatBytes)
	return true
}

func (r *Relay) chatInvite(w http.ResponseWriter, req *http.Request, room, action string) {
	var body chatBody
	if !readJSON(w, req, 2*MaxChatCiphertext, &body) || (action == "decline" && !body.sealed(true)) {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	token := bearer(req)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireChatsLocked()
	c := r.chats[room]
	// The room id is itself derived from the code, so reporting "ended" or
	// "already joined" reveals nothing to someone without it.
	switch {
	case c == nil:
		http.NotFound(w, req)
		return
	case c.state == chatEnded:
		http.Error(w, "chat ended", http.StatusGone)
		return
	case !validToken(token) || !equal(token, c.invite):
		http.NotFound(w, req)
		return
	case c.state == chatActive || c.state == chatPaused:
		// A retried join with the seat's own guest token succeeds, so a
		// guest whose first response was lost can still take its seat.
		if action == "join" && validToken(body.Guest) && equal(body.Guest, c.guest) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "chat already joined", http.StatusConflict)
		return
	}
	switch action {
	case "opened":
		if c.state == chatPending {
			r.pushLocked(c, "host", chatEvent{Type: "opened"})
			r.setStateLocked(c, chatOpened)
			r.log.Info("chat opened", "room", tag(room))
		}
	case "join":
		if !validToken(body.Guest) || body.Guest == c.host || body.Guest == c.invite {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if c.doneRule == "any" && !slices.Contains(body.Features, "charter") {
			http.Error(w, "this chat uses a workflow that needs jand 0.4.1 or later; upgrade before joining", http.StatusConflict)
			return
		}
		c.guest = body.Guest
		r.pushLocked(c, "host", chatEvent{Type: "joined"})
		r.setStateLocked(c, chatActive)
		r.log.Info("chat joined", "room", tag(room))
	case "decline":
		if r.roomFull(room, len(body.Data)) {
			http.Error(w, "relay full", http.StatusServiceUnavailable)
			return
		}
		r.endChatLocked(room, c, chatEvent{Type: "declined", From: "guest", Ctr: body.Ctr, Data: body.Data}, "host")
	}
	w.WriteHeader(http.StatusNoContent)
}

// chatParty handles every request made with a party token after the join.
func (r *Relay) chatParty(w http.ResponseWriter, req *http.Request, room, action string) {
	var body chatBody
	valid := readJSON(w, req, 2*MaxChatCiphertext, &body)
	switch action {
	case "messages", "propose":
		valid = valid && body.sealed(false)
	case "checkpoint", "done":
		valid = valid && body.sealed(true)
	}
	if action == "propose" && body.Budget == 0 {
		body.Budget = r.config.ChatDefaultBudget
	}
	if !valid || action == "propose" && !r.validBudget(body.Budget) {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	token := bearer(req)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireChatsLocked()
	c := r.chats[room]
	var from string
	if c != nil {
		from = c.role(token)
	}
	if from == "" {
		http.NotFound(w, req)
		return
	}
	if action == "close" {
		if c.state != chatEnded {
			r.endChatLocked(room, c, chatEvent{Type: "closed", From: from}, "host", "guest")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch c.state {
	case chatEnded:
		http.Error(w, "chat ended", http.StatusGone)
		return
	case chatPending, chatOpened:
		http.Error(w, "peer has not joined", http.StatusConflict)
		return
	}
	if r.roomFull(room, 2*len(body.Data)) {
		http.Error(w, "relay full", http.StatusServiceUnavailable)
		return
	}
	peer := other(from)
	switch action {
	case "messages":
		if c.state == chatPaused {
			// A few closing replies or notes may pass, outside the budget, so
			// messages that crossed the pause can still be answered. Nothing
			// that asks for more work does.
			if !body.Closing {
				http.Error(w, "chat paused at a checkpoint; only closing replies or notes may be sent until a new goal is accepted", http.StatusConflict)
				return
			}
			if c.pauseNotes[from] >= c.pauseNotesMax {
				http.Error(w, "closing message limit reached for this pause", http.StatusConflict)
				return
			}
			if c.messages >= r.config.ChatMaxMessages {
				r.endChatLocked(room, c, chatEvent{Type: "expired", Reason: "limit"}, "host", "guest")
				http.Error(w, "chat message limit reached", http.StatusGone)
				return
			}
			e := r.pushLocked(c, peer, chatEvent{Type: "message", From: from, Ctr: body.Ctr, Data: body.Data, Paused: true})
			c.pauseNotes[from]++
			c.messages++
			c.wake()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]uint64{"seq": e.Seq})
			return
		}
		if c.messages >= r.config.ChatMaxMessages {
			r.endChatLocked(room, c, chatEvent{Type: "expired", Reason: "limit"}, "host", "guest")
			http.Error(w, "chat message limit reached", http.StatusGone)
			return
		}
		e := r.pushLocked(c, peer, chatEvent{Type: "message", From: from, Ctr: body.Ctr, Data: body.Data})
		c.messages++
		c.used++
		c.changed = time.Now()
		if c.used >= c.budget {
			// The budget is the backstop for an agent that never decides it is
			// done: the relay pauses the chat whatever the agents think.
			for _, role := range []string{"host", "guest"} {
				r.pushLocked(c, role, chatEvent{Type: "checkpoint", Reason: "budget"})
			}
			c.state = chatPaused
			r.log.Info("chat checkpoint", "room", tag(room), "reason", "budget", "used", c.used)
		}
		c.wake()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]uint64{"seq": e.Seq})
		return
	case "checkpoint":
		if c.state != chatActive {
			http.Error(w, "chat is already paused", http.StatusConflict)
			return
		}
		r.pushLocked(c, peer, chatEvent{Type: "checkpoint", From: from, Reason: "goal_reached", Ctr: body.Ctr, Data: body.Data})
		r.setStateLocked(c, chatPaused)
		r.log.Info("chat checkpoint", "room", tag(room), "reason", "goal_reached", "by", from)
	case "done":
		// One party finishing is news, not a pause: the other may still be
		// working or need something. The pause comes when both are done.
		if c.state != chatActive {
			http.Error(w, "chat is paused", http.StatusConflict)
			return
		}
		if c.done[from] {
			http.Error(w, "already reported done for this goal", http.StatusConflict)
			return
		}
		c.done[from] = true
		r.pushLocked(c, peer, chatEvent{Type: "done", From: from, Ctr: body.Ctr, Data: body.Data})
		c.changed = time.Now()
		if c.done[peer] || c.doneRule == "any" {
			reason := "all_done"
			if !c.done[peer] {
				reason = "any_done" // one side works, the other accepts
			}
			for _, role := range []string{"host", "guest"} {
				r.pushLocked(c, role, chatEvent{Type: "checkpoint", Reason: reason})
			}
			c.state = chatPaused
			r.log.Info("chat checkpoint", "room", tag(room), "reason", reason)
		} else {
			r.log.Info("chat party done", "room", tag(room), "by", from)
		}
		c.wake()
	case "propose":
		if c.state != chatPaused {
			http.Error(w, "goals can only be proposed at a checkpoint", http.StatusConflict)
			return
		}
		e := r.pushLocked(c, peer, chatEvent{Type: "proposal", From: from, Ctr: body.Ctr, Data: body.Data, Budget: body.Budget})
		c.proposal = &e // a newer proposal from either side replaces this one
		c.changed = time.Now()
		c.wake()
		r.log.Info("chat goal proposed", "room", tag(room), "by", from, "budget", body.Budget)
	case "accept":
		p := c.proposal
		if c.state != chatPaused || p == nil || p.From == from || p.Seq != body.Seq {
			http.Error(w, "no matching proposal to accept", http.StatusConflict)
			return
		}
		for _, role := range []string{"host", "guest"} {
			r.pushLocked(c, role, chatEvent{Type: "resumed", From: p.From, Ctr: p.Ctr, Data: p.Data, Budget: p.Budget})
		}
		c.budget, c.used, c.proposal = p.Budget, 0, nil
		clear(c.done)
		clear(c.pauseNotes)
		r.setStateLocked(c, chatActive)
		r.log.Info("chat resumed", "room", tag(room), "accepted_by", from, "budget", c.budget)
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxCharter bounds the sealed charter: workflow instructions (8 KiB), goal
// and the rest, base64-encoded.
const maxCharter = 16 * 1024

func validCharter(data string) bool {
	raw, err := base64.StdEncoding.DecodeString(data)
	return err == nil && len(raw) >= 29 && len(data) <= maxCharter
}

// chatCharter returns the host's sealed charter with the terms this relay
// enforces. The guest reads it with the invite token before joining, so its
// user can accept the workflow together with the goal; parties may reread it.
func (r *Relay) chatCharter(w http.ResponseWriter, req *http.Request, room string) {
	token := bearer(req)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireChatsLocked()
	c := r.chats[room]
	if c == nil || !validToken(token) || !(c.role(token) != "" || c.invite != "" && equal(token, c.invite)) {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{"charter": c.charter, "done_rule": c.doneRule,
		"pause_notes": c.pauseNotesMax, "budget": c.budget})
}

// chatEvents long-polls one party's queue. Events up to `after` are
// acknowledged and released; the rest are returned without being consumed,
// so a response lost in transit is simply fetched again.
func (r *Relay) chatEvents(w http.ResponseWriter, req *http.Request, room string) {
	q := req.URL.Query()
	after, err := strconv.ParseUint(q.Get("after"), 10, 64)
	if q.Get("after") == "" {
		after, err = 0, nil
	}
	wait := 0
	if v := q.Get("wait"); v != "" && err == nil {
		wait, err = strconv.Atoi(v)
	}
	maxWait := int(r.config.ChatMaxWait / time.Second)
	if err != nil || wait < 0 || wait > maxWait {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	token := bearer(req)
	deadline := time.NewTimer(time.Duration(wait) * time.Second)
	defer deadline.Stop()
	r.mu.Lock()
	waiting := false
	defer func() {
		if waiting {
			if c := r.chats[room]; c != nil {
				c.waiters--
			}
		}
		r.mu.Unlock()
	}()
	for {
		r.expireChatsLocked()
		c := r.chats[room]
		var role string
		if c != nil {
			role = c.role(token)
		}
		if role == "" {
			http.NotFound(w, req)
			return
		}
		kept := c.queue[role][:0]
		for _, e := range c.queue[role] {
			if e.Seq > after {
				kept = append(kept, e)
			} else {
				c.bytes -= int64(len(e.Data))
				r.chatBytes -= int64(len(e.Data))
			}
		}
		c.queue[role] = kept
		if len(kept) > 0 || c.state == chatEnded || wait == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(struct {
				State  string      `json:"state"`
				Events []chatEvent `json:"events"`
			}{c.state, append([]chatEvent{}, kept...)})
			return
		}
		if !waiting {
			if c.waiters >= maxChatWaiters {
				http.Error(w, "too many waiters", http.StatusTooManyRequests)
				return
			}
			c.waiters++
			waiting = true
		}
		notify := c.notify
		r.mu.Unlock()
		select {
		case <-notify:
		case <-deadline.C:
			wait = 0
		case <-req.Context().Done():
			wait = 0
		}
		r.mu.Lock()
		if r.chats[room] != c {
			// Room was deleted while waiting; the deferred decrement must not
			// touch a replacement room with the same id.
			waiting = false
		}
	}
}
