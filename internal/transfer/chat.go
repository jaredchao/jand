package transfer

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jaredchao/jand/internal/code"
)

const (
	chatProtocol = "jand-chat/1"
	MaxChatText  = 64 * 1024
	// MaxChatGoal keeps the goal inside the packet header 0.2.x can read.
	MaxChatGoal = 1024
	// MaxChatBudget matches the relay's per-chat message limit.
	MaxChatBudget = 200
	// One long-poll request; the relay caps it at 20 seconds, below the
	// client's 30-second response header timeout and common proxy timeouts.
	chatPoll = 20 * time.Second
)

var (
	ErrChatEnded    = errors.New("chat has ended")
	ErrChatNotFound = errors.New("chat not found or expired on the relay")
)

// chatState is everything a later process needs to act in one chat. It holds
// the message key, so it is written with owner-only permissions.
type chatState struct {
	ID    string `json:"id"`
	Relay string `json:"relay"`
	Role  string `json:"role"` // host or guest
	Token string `json:"token"`
	Key   string `json:"key"`
}

// cursor and counter live in separate files so a background recv and a
// foreground send never overwrite each other's progress.
type chatCursor struct {
	After   uint64 `json:"after"`
	PeerCtr uint64 `json:"peer_ctr"`
	// The peer's latest goal proposal, which accept refers to by sequence.
	Proposal       uint64 `json:"proposal,omitempty"`
	ProposalGoal   string `json:"proposal_goal,omitempty"`
	ProposalBudget int    `json:"proposal_budget,omitempty"`
}

type chatCounter struct {
	Ctr uint64 `json:"ctr"`
}

func DefaultStateDir() string {
	if v := os.Getenv("JAND_HOME"); v != "" {
		return filepath.Join(v, "chats")
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "jand", "chats")
	}
	return filepath.Join(".jand", "chats")
}

func chatRoomID(c code.Code) string {
	id := c.Derive("chat/room")
	return hex.EncodeToString(id[:16])
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func (s *chatState) peer() string {
	if s.Role == "host" {
		return "guest"
	}
	return "host"
}

func (s *chatState) path(dir, suffix string) string {
	return filepath.Join(dir, s.ID+suffix)
}

func writeJSONFile(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// loadChat resolves a full id or a unique prefix of at least 8 characters.
func loadChat(dir, id string) (*chatState, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) < 8 || len(id) > 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return nil, errors.New("chat id must be at least 8 hex characters")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, id+"*.json"))
	var found []string
	for _, m := range matches {
		if name := strings.TrimSuffix(filepath.Base(m), ".json"); len(name) == 32 {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("no local chat %s in %s", id, dir)
	case 1:
	default:
		return nil, fmt.Errorf("chat id %s is ambiguous; give more characters", id)
	}
	var s chatState
	if err := readJSONFile(found[0], &s); err != nil || len(s.ID) != 32 || (s.Role != "host" && s.Role != "guest") {
		return nil, fmt.Errorf("unreadable chat state %s", found[0])
	}
	return &s, nil
}

func (s *chatState) key() ([32]byte, error) {
	var k [32]byte
	b, err := base64.RawURLEncoding.DecodeString(s.Key)
	if err != nil || len(b) != len(k) {
		return k, errors.New("corrupt chat key")
	}
	copy(k[:], b)
	return k, nil
}

func chatAAD(room, from, kind string, ctr uint64) []byte {
	return []byte(fmt.Sprintf("%s/%s/%s/%s/%d", chatProtocol, room, from, kind, ctr))
}

func chatAEAD(key [32]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealChat(key [32]byte, room, from, kind string, ctr uint64, text string) (string, error) {
	aead, err := chatAEAD(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(text), chatAAD(room, from, kind, ctr))), nil
}

func openChat(key [32]byte, room, from, kind string, ctr uint64, data string) (string, error) {
	bad := errors.New("chat message failed authentication")
	blob, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", bad
	}
	aead, err := chatAEAD(key)
	if err != nil || len(blob) < aead.NonceSize()+aead.Overhead() {
		return "", bad
	}
	n := aead.NonceSize()
	plain, err := aead.Open(nil, blob[:n], blob[n:], chatAAD(room, from, kind, ctr))
	if err != nil || len(plain) > MaxChatText {
		return "", bad
	}
	return strings.ToValidUTF8(string(plain), "�"), nil
}

func checkGoal(goal string, budget int) error {
	if strings.TrimSpace(goal) == "" {
		return errors.New("a chat needs a goal: say what counts as done (--goal)")
	}
	if len(goal) > MaxChatGoal || !utf8.ValidString(goal) {
		return fmt.Errorf("goal must be valid UTF-8 of at most %d bytes", MaxChatGoal)
	}
	if budget < 0 || budget > MaxChatBudget {
		return fmt.Errorf("budget must be between 1 and %d messages", MaxChatBudget)
	}
	return nil
}

func checkText(text string) error {
	if text == "" {
		return errors.New("message is empty")
	}
	if len(text) > MaxChatText {
		return fmt.Errorf("message exceeds %d bytes", MaxChatText)
	}
	if !utf8.ValidString(text) {
		return errors.New("message is not valid UTF-8")
	}
	return nil
}

// chatCall sends one JSON request and maps the relay's chat statuses to errors.
func chatCall(ctx context.Context, relay, room, action, token string, body any) (*http.Response, error) {
	p := "/v1/chats/" + room
	if action != "" {
		p += "/" + action
	}
	target, err := relayURL(relay, p)
	if err != nil {
		return nil, err
	}
	var payload []byte
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	method := http.MethodPost
	if action == "" {
		method = http.MethodPut
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach relay: %w", err)
	}
	return resp, nil
}

func chatError(resp *http.Response) error {
	reason, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrChatNotFound
	case http.StatusGone:
		return ErrChatEnded
	}
	if r := relayReason(reason); r != "" {
		return fmt.Errorf("relay rejected chat request: HTTP %d: %s", resp.StatusCode, r)
	}
	return fmt.Errorf("relay rejected chat request: HTTP %d", resp.StatusCode)
}

func createChat(ctx context.Context, c code.Code, o Options) (*chatState, error) {
	host, err := randomToken()
	if err != nil {
		return nil, err
	}
	key := c.Derive("chat/key")
	s := &chatState{ID: chatRoomID(c), Relay: o.RelayURL, Role: "host", Token: host,
		Key: base64.RawURLEncoding.EncodeToString(key[:])}
	resp, err := chatCall(ctx, o.RelayURL, s.ID, "", "", map[string]any{"host": host, "invite": c.Token("chat/invite"), "budget": o.Budget})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, errors.New("relay does not support chat; upgrade the relay")
	}
	if resp.StatusCode != http.StatusCreated {
		return nil, chatError(resp)
	}
	resp.Body.Close()
	if err = writeJSONFile(s.path(o.StateDir, ".json"), s); err != nil {
		return nil, fmt.Errorf("cannot save chat state: %w", err)
	}
	return s, nil
}

// abandon closes a room whose invitation packet never reached the relay.
func (s *chatState) abandon(o Options) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if resp, err := chatCall(ctx, s.Relay, s.ID, "close", s.Token, nil); err == nil {
		resp.Body.Close()
	}
	os.Remove(s.path(o.StateDir, ".json"))
}

// reportOpened tells the host the packet was claimed. It is best effort: a
// failure must not turn a saved file into a receive error.
func reportOpened(ctx context.Context, c code.Code, o Options) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if resp, err := chatCall(ctx, o.RelayURL, chatRoomID(c), "opened", c.Token("chat/invite"), nil); err == nil {
		resp.Body.Close()
	}
}

// ChatJoin accepts an invitation. Call it only after the local user agreed.
func ChatJoin(ctx context.Context, rawCode string, opts Options) (string, error) {
	o := opts.defaults()
	c, err := parseInviteCode(rawCode)
	if err != nil {
		return "", err
	}
	id := chatRoomID(c)
	statePath := filepath.Join(o.StateDir, id+".json")
	var s *chatState
	if prior, err := loadChat(o.StateDir, id); err == nil {
		if prior.Role != "guest" {
			return "", fmt.Errorf("chat %s was started from this state directory (%s); to test both sides on one machine, give each side its own JAND_HOME", id[:8], o.StateDir)
		}
		// A retry: the relay accepts the same guest token again, so a join
		// whose response was lost can still take the seat it locked. The
		// relay given now wins, in case the first attempt used a wrong one.
		s = prior
		if s.Relay != o.RelayURL {
			s.Relay = o.RelayURL
			if err := writeJSONFile(statePath, s); err != nil {
				return "", fmt.Errorf("cannot save chat state: %w", err)
			}
		}
	} else {
		guest, err := randomToken()
		if err != nil {
			return "", err
		}
		key := c.Derive("chat/key")
		s = &chatState{ID: id, Relay: o.RelayURL, Role: "guest", Token: guest,
			Key: base64.RawURLEncoding.EncodeToString(key[:])}
		// Saved before the request so the token survives a lost response.
		if err = writeJSONFile(statePath, s); err != nil {
			return "", fmt.Errorf("cannot save chat state: %w", err)
		}
	}
	resp, err := chatCall(ctx, s.Relay, s.ID, "join", c.Token("chat/invite"), map[string]string{"guest": s.Token})
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusNoContent {
		// Only a definite refusal discards the token. A proxy's 5xx may hide a
		// join that did lock the seat, and a retry needs the same token.
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusGone:
			os.Remove(statePath)
		}
		return "", chatError(resp)
	}
	resp.Body.Close()
	o.Emit(Event{Event: "joined", Chat: s.ID, Transcript: s.path(o.StateDir, ".transcript.jsonl")})
	return s.ID, nil
}

// ChatDecline refuses an invitation, optionally with a short reason.
func ChatDecline(ctx context.Context, rawCode, reason string, opts Options) error {
	o := opts.defaults()
	c, err := parseInviteCode(rawCode)
	if err != nil {
		return err
	}
	body := map[string]any{}
	if reason != "" {
		if err = checkText(reason); err != nil {
			return err
		}
		data, err := sealChat(c.Derive("chat/key"), chatRoomID(c), "guest", "decline", 1, reason)
		if err != nil {
			return err
		}
		body["ctr"], body["data"] = 1, data
	}
	resp, err := chatCall(ctx, o.RelayURL, chatRoomID(c), "decline", c.Token("chat/invite"), body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent {
		return chatError(resp)
	}
	resp.Body.Close()
	o.Emit(Event{Event: "declined", Chat: chatRoomID(c)})
	return nil
}

type transcriptLine struct {
	Time string `json:"time"`
	From string `json:"from"`
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

func (s *chatState) record(dir, from, kind, text string) {
	line, _ := json.Marshal(transcriptLine{time.Now().UTC().Format(time.RFC3339), from, kind, text})
	f, err := os.OpenFile(s.path(dir, ".transcript.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return
	}
	f.Write(append(line, '\n'))
	f.Close()
}

func ChatSend(ctx context.Context, id, text string, opts Options) error {
	o := opts.defaults()
	s, err := loadChat(o.StateDir, id)
	if err != nil {
		return err
	}
	if err = checkText(text); err != nil {
		return err
	}
	key, err := s.key()
	if err != nil {
		return err
	}
	body, err := s.seal(o.StateDir, key, "message", text)
	if err != nil {
		return err
	}
	if err = s.post(ctx, "messages", body, http.StatusCreated); err != nil {
		return err
	}
	s.record(o.StateDir, s.Role, "message", text)
	o.Emit(Event{Event: "sent", Chat: s.ID, Size: int64(len(text))})
	return nil
}

// parseInviteCode explains the one mistake the saved event used to invite:
// joining takes the 43-character code, not the 32-character chat id.
func parseInviteCode(raw string) (code.Code, error) {
	c, err := code.Parse(raw)
	if err != nil && len(strings.TrimSpace(raw)) == 32 && code.ValidRoom(strings.ToLower(strings.TrimSpace(raw))) {
		return c, errors.New("join and decline need the 43-character receive code, not the chat id")
	}
	return c, err
}

// seal encrypts one payload under the next local counter. The counter is
// persisted first, so a retry after a lost response never reuses a value
// the peer may already have seen.
func (s *chatState) seal(dir string, key [32]byte, kind, text string) (map[string]any, error) {
	var counter chatCounter
	readJSONFile(s.path(dir, ".counter"), &counter)
	counter.Ctr++
	if err := writeJSONFile(s.path(dir, ".counter"), counter); err != nil {
		return nil, err
	}
	data, err := sealChat(key, s.ID, s.Role, kind, counter.Ctr, text)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ctr": counter.Ctr, "data": data}, nil
}

func (s *chatState) post(ctx context.Context, action string, body any, want int) error {
	resp, err := chatCall(ctx, s.Relay, s.ID, action, s.Token, body)
	if err != nil {
		return err
	}
	if resp.StatusCode != want {
		return chatError(resp)
	}
	resp.Body.Close()
	return nil
}

// ChatCheckpoint reports that this side considers the goal reached. The chat
// pauses until a new goal is proposed and accepted, or someone closes it.
func ChatCheckpoint(ctx context.Context, id, summary string, opts Options) error {
	o := opts.defaults()
	s, err := loadChat(o.StateDir, id)
	if err != nil {
		return err
	}
	body := map[string]any{}
	if summary != "" {
		key, err := s.key()
		if err == nil {
			err = checkText(summary)
		}
		if err == nil {
			body, err = s.seal(o.StateDir, key, "checkpoint", summary)
		}
		if err != nil {
			return err
		}
	}
	if err = s.post(ctx, "checkpoint", body, http.StatusNoContent); err != nil {
		return err
	}
	s.record(o.StateDir, s.Role, "checkpoint", summary)
	o.Emit(Event{Event: "checkpoint", Chat: s.ID, By: "self", Reason: "goal_reached", Text: summary})
	return nil
}

// ChatPropose offers the next goal at a checkpoint. Call it only with the
// local user's agreement; the peer's user must accept it too.
func ChatPropose(ctx context.Context, id, goal string, budget int, opts Options) error {
	o := opts.defaults()
	s, err := loadChat(o.StateDir, id)
	if err != nil {
		return err
	}
	if err = checkGoal(goal, budget); err != nil {
		return err
	}
	key, err := s.key()
	if err != nil {
		return err
	}
	body, err := s.seal(o.StateDir, key, "proposal", goal)
	if err != nil {
		return err
	}
	body["budget"] = budget
	if err = s.post(ctx, "propose", body, http.StatusNoContent); err != nil {
		return err
	}
	s.record(o.StateDir, s.Role, "proposal", goal)
	o.Emit(Event{Event: "proposed", Chat: s.ID, Goal: goal, Budget: budget})
	return nil
}

// ChatAccept accepts the peer's latest proposal, as last seen by recv.
func ChatAccept(ctx context.Context, id string, opts Options) error {
	o := opts.defaults()
	s, err := loadChat(o.StateDir, id)
	if err != nil {
		return err
	}
	var cur chatCursor
	readJSONFile(s.path(o.StateDir, ".cursor"), &cur)
	if cur.Proposal == 0 {
		return errors.New("no goal proposal from the peer to accept; run recv first")
	}
	if err = s.post(ctx, "accept", map[string]any{"seq": cur.Proposal}, http.StatusNoContent); err != nil {
		return err
	}
	o.Emit(Event{Event: "accepted", Chat: s.ID, Goal: cur.ProposalGoal, Budget: cur.ProposalBudget})
	return nil
}

func ChatClose(ctx context.Context, id string, opts Options) error {
	o := opts.defaults()
	s, err := loadChat(o.StateDir, id)
	if err != nil {
		return err
	}
	resp, err := chatCall(ctx, s.Relay, s.ID, "close", s.Token, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent {
		return chatError(resp)
	}
	resp.Body.Close()
	s.record(o.StateDir, s.Role, "closed", "")
	o.Emit(Event{Event: "closed", Chat: s.ID, By: "self"})
	return nil
}

type relayEvent struct {
	Seq    uint64 `json:"seq"`
	Type   string `json:"type"`
	From   string `json:"from"`
	Ctr    uint64 `json:"ctr"`
	Data   string `json:"data"`
	Reason string `json:"reason"`
	Budget int    `json:"budget"`
}

// ChatRecv emits pending events. With wait > 0 it blocks until at least one
// event arrives or wait elapses, in which case it emits no_events.
func ChatRecv(ctx context.Context, id string, wait time.Duration, opts Options) error {
	o := opts.defaults()
	s, err := loadChat(o.StateDir, id)
	if err != nil {
		return err
	}
	key, err := s.key()
	if err != nil {
		return err
	}
	var cur chatCursor
	readJSONFile(s.path(o.StateDir, ".cursor"), &cur)
	deadline := time.Now().Add(wait)
	for {
		poll := min(time.Until(deadline), chatPoll)
		if poll < time.Second {
			poll = 0
		}
		target, err := relayURL(s.Relay, "/v1/chats/"+s.ID+"/events")
		if err != nil {
			return err
		}
		target += "?after=" + strconv.FormatUint(cur.After, 10) + "&wait=" + strconv.Itoa(int(poll/time.Second))
		resp, err := request(ctx, http.MethodGet, target, s.Token, nil)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("cannot reach relay: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return chatError(resp)
		}
		var page struct {
			State  string       `json:"state"`
			Events []relayEvent `json:"events"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("invalid relay response: %w", err)
		}
		if len(page.Events) > 0 {
			for _, e := range page.Events {
				if e.Seq <= cur.After {
					continue
				}
				o.Emit(s.translate(o.StateDir, key, &cur, e))
				cur.After = e.Seq
			}
			return writeJSONFile(s.path(o.StateDir, ".cursor"), cur)
		}
		if page.State == "ended" {
			return ErrChatEnded
		}
		if poll == 0 {
			o.Emit(Event{Event: "no_events", Chat: s.ID})
			return nil
		}
	}
}

// openPeer authenticates a payload the peer sealed and advances its counter.
// It returns a reason instead of an error so the caller can emit it.
func (s *chatState) openPeer(key [32]byte, cur *chatCursor, e relayEvent, kind string) (string, string) {
	if e.From != s.peer() || e.Ctr <= cur.PeerCtr {
		return "", "dropped a " + kind + " with an unexpected sender or replayed counter"
	}
	text, err := openChat(key, s.ID, e.From, kind, e.Ctr, e.Data)
	if err != nil {
		return "", "dropped a " + kind + " that failed authentication"
	}
	// A decline ends the chat, so its counter never needs to be tracked.
	if kind != "decline" {
		cur.PeerCtr = e.Ctr
	}
	return text, ""
}

// translate turns one relay event into a local event, verifying message
// direction, authenticity and ordering. The relay is not trusted.
func (s *chatState) translate(dir string, key [32]byte, cur *chatCursor, e relayEvent) Event {
	out := Event{Chat: s.ID}
	fail := func(msg string) Event {
		out.Event, out.Message = "error", msg
		return out
	}
	switch e.Type {
	case "opened", "joined":
		if s.Role != "host" {
			return fail("unexpected " + e.Type + " event from relay")
		}
		out.Event = e.Type
	case "expired":
		out.Event, out.Reason = "expired", e.Reason
		s.record(dir, "relay", "expired", e.Reason)
	case "closed":
		out.Event, out.By = "closed", "peer"
		if e.From == s.Role {
			out.By = "self"
		} else {
			s.record(dir, s.peer(), "closed", "")
		}
	case "checkpoint":
		out.Event, out.Reason = "checkpoint", e.Reason
		if e.Reason == "budget" {
			// Relay-enforced: the goal's message budget is spent.
			out.By = "relay"
			s.record(dir, "relay", "checkpoint", "budget")
			return out
		}
		out.By, out.From = "peer", s.peer()
		if e.From != s.peer() {
			return fail("unexpected checkpoint event from relay")
		}
		if e.Data == "" {
			s.record(dir, s.peer(), "checkpoint", "")
			return out
		}
		text, bad := s.openPeer(key, cur, e, "checkpoint")
		if bad != "" {
			return fail(bad)
		}
		out.Text, out.Untrusted = text, true
		s.record(dir, s.peer(), "checkpoint", text)
	case "proposal":
		goal, bad := s.openPeer(key, cur, e, "proposal")
		if bad != "" {
			return fail(bad)
		}
		cur.Proposal, cur.ProposalGoal, cur.ProposalBudget = e.Seq, goal, e.Budget
		out.Event, out.From, out.Goal, out.Budget, out.Untrusted = "proposal", s.peer(), goal, e.Budget, true
		s.record(dir, s.peer(), "proposal", goal)
	case "resumed":
		// Carries the accepted proposal, which may be this side's own; its
		// counter was checked when the proposal itself arrived.
		if e.From != "host" && e.From != "guest" {
			return fail("unexpected resumed event from relay")
		}
		goal, err := openChat(key, s.ID, e.From, "proposal", e.Ctr, e.Data)
		if err != nil {
			return fail("dropped a goal that failed authentication")
		}
		cur.Proposal, cur.ProposalGoal, cur.ProposalBudget = 0, "", 0
		out.Event, out.Goal, out.Budget, out.By = "resumed", goal, e.Budget, "peer"
		if e.From == s.Role {
			out.By = "self"
		}
		s.record(dir, "relay", "resumed", goal)
	case "message", "declined":
		kind := "message"
		if e.Type == "declined" {
			kind = "decline"
			if s.Role != "host" {
				return fail("unexpected declined event from relay")
			}
		}
		out.Event, out.From = e.Type, s.peer()
		if e.Type == "declined" && e.Data == "" {
			s.record(dir, s.peer(), "declined", "")
			return out
		}
		text, bad := s.openPeer(key, cur, e, kind)
		if bad != "" {
			return fail(bad)
		}
		out.Text, out.Untrusted = text, true
		s.record(dir, s.peer(), e.Type, text)
	default:
		return fail("unknown event from relay")
	}
	return out
}
