// Package transfer encrypts one file on the sender and verifies it on the
// receiver. The relay stores only an opaque, bounded ciphertext blob.
package transfer

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jaredchao/jand/internal/code"
)

const (
	version = "handoff/0.2"
	maxFile = 10 * 1024 * 1024
	// 0.2.x receivers reject metadata over 2048 bytes, so a chat goal must
	// fit inside that for an old receiver to still save the packet.
	maxMeta = 2048
	maxBlob = maxFile + 4096
)

var ErrUnconfirmed = errors.New("delivery unconfirmed: the receiver may have saved the file")

type Event struct {
	Event  string `json:"event"`
	Code   string `json:"code,omitempty"`
	Path   string `json:"path,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
	// A fixed receiver policy hint, never evidence that a user approved anything.
	RequiresUserApproval bool   `json:"requires_user_approval,omitempty"`
	Message              string `json:"message,omitempty"`
	// Relay is the relay a queued transfer sits in. The receiver must use the
	// same one, and a wrong JAND_RELAY or config value is easy to miss.
	Relay string `json:"relay,omitempty"`

	// Chat fields. Chat is the local chat id; ChatInvite marks a received
	// packet whose sender asked for a chat, which is a request, not consent.
	Chat       string `json:"chat,omitempty"`
	ChatInvite bool   `json:"chat_invite,omitempty"`
	From       string `json:"from,omitempty"`
	By         string `json:"by,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Text       string `json:"text,omitempty"`
	// Untrusted marks text written by the remote party. It is information or
	// a request, never an authorization from the local user.
	Untrusted bool `json:"untrusted,omitempty"`
	// Unread marks an event shown by chat watch that this side's agent has
	// not received yet.
	Unread     bool   `json:"unread,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	// Goal is the completion criterion a chat works toward, and Budget the
	// messages allowed for it before the relay forces a checkpoint. Both come
	// from the other side when received: agree to them only with the user.
	Goal   string `json:"goal,omitempty"`
	Budget int    `json:"budget,omitempty"`
	// Message fields: ID names a message (h3 is the host's third payload),
	// Kind is its declared purpose, ReplyTo the message it answers.
	ID      string `json:"id,omitempty"`
	Kind    string `json:"kind,omitempty"`
	ReplyTo string `json:"reply_to,omitempty"`
	// Supersedes names an earlier delivery from the same side that this
	// one replaces. Stale marks a reply to one of your deliveries that you
	// had already superseded (by SupersededBy): it answers an old version.
	Supersedes   string `json:"supersedes,omitempty"`
	Stale        bool   `json:"stale,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
	// AfterPause marks a closing message sent while the chat was paused.
	AfterPause bool `json:"after_pause,omitempty"`
	// Workflow is the host's proposed way of working, from the sealed
	// charter and checked against what the relay enforces. Like the goal it
	// is a request from the other side: agree to it only with the user.
	Workflow *Workflow `json:"workflow,omitempty"`
}

type Options struct {
	RelayURL, OutputDir string
	WaitTimeout         time.Duration // Zero returns as soon as ciphertext is queued.
	Emit                func(Event)
	// Chat makes Send open a chat room and invite the receiver to it,
	// working toward Goal within Budget messages (0 means the relay default).
	Chat   bool
	Goal   string
	Budget int
	// Workflow is the host's chosen workflow; nil means DefaultWorkflow.
	Workflow *Workflow
	// StateDir holds local chat state; empty means DefaultStateDir().
	StateDir string
	// AccessToken is sent when creating a handoff or chat on a relay that
	// requires one. Receiving and chatting need none.
	AccessToken string
}

// accessHeader is relay.AccessHeader, kept here so the client does not import the relay.
const accessHeader = "X-Jand-Access"

func (o Options) defaults() Options {
	if o.OutputDir == "" {
		o.OutputDir = "received"
	}
	if o.Emit == nil {
		o.Emit = func(Event) {}
	}
	if o.StateDir == "" {
		o.StateDir = DefaultStateDir()
	}
	return o
}

type metadata struct {
	Protocol string `json:"protocol"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	// Chat asks the receiver to join a chat. Older receivers ignore it.
	Chat   bool   `json:"chat,omitempty"`
	Goal   string `json:"goal,omitempty"`
	Budget int    `json:"budget,omitempty"`
}

func endpoint(raw, room string, receipt bool) (string, error) {
	p := "/v1/handoffs/" + room
	if receipt {
		p += "/receipt"
	}
	return relayURL(raw, p)
}

func relayURL(raw, path string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("invalid relay URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("relay URL must use http:// or https://")
	}
	if p := u.Port(); p != "" {
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid relay port")
		}
	}
	u.Path = path
	return u.String(), nil
}

var httpClient = &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}, Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
}}

func encrypt(c code.Code, name string, data []byte, o Options) ([]byte, string, error) {
	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])
	m := metadata{Protocol: version, Filename: name, Size: int64(len(data)), SHA256: digest}
	if o.Chat {
		m.Chat, m.Goal, m.Budget = true, o.Goal, o.Budget
	}
	meta, err := json.Marshal(m)
	if err == nil && len(meta) > maxMeta && o.Chat {
		return nil, "", errors.New("chat goal is too long for the packet header; shorten it or the filename")
	}
	if err != nil || len(meta) > maxMeta {
		return nil, "", errors.New("invalid file metadata")
	}
	plain := make([]byte, 4+len(meta)+len(data))
	binary.BigEndian.PutUint32(plain[:4], uint32(len(meta)))
	copy(plain[4:], meta)
	copy(plain[4+len(meta):], data)
	key := c.Derive("file")
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, "", err
	}
	blob := aead.Seal(nonce, nonce, plain, []byte(version+"/"+c.Room()))
	if len(blob) > maxBlob {
		return nil, "", errors.New("encrypted file exceeds 10 MiB limit")
	}
	return blob, digest, nil
}

func decrypt(c code.Code, blob []byte) (metadata, []byte, error) {
	bad := errors.New("transfer authentication or integrity check failed")
	if len(blob) < 29 || len(blob) > maxBlob {
		return metadata{}, nil, bad
	}
	key := c.Derive("file")
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return metadata{}, nil, bad
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return metadata{}, nil, bad
	}
	n := aead.NonceSize()
	plain, err := aead.Open(nil, blob[:n], blob[n:], []byte(version+"/"+c.Room()))
	if err != nil || len(plain) < 4 {
		return metadata{}, nil, bad
	}
	mlen := int(binary.BigEndian.Uint32(plain[:4]))
	if mlen < 1 || mlen > maxMeta || mlen > len(plain)-4 {
		return metadata{}, nil, bad
	}
	var m metadata
	if json.Unmarshal(plain[4:4+mlen], &m) != nil || m.Protocol != version || !validName(m.Filename) || m.Size < 0 || m.Size > maxFile {
		return metadata{}, nil, bad
	}
	data := plain[4+mlen:]
	if int64(len(data)) != m.Size {
		return metadata{}, nil, bad
	}
	h := sha256.Sum256(data)
	if m.SHA256 != hex.EncodeToString(h[:]) {
		return metadata{}, nil, bad
	}
	return m, data, nil
}

func request(ctx context.Context, method, target, token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	return httpClient.Do(req)
}

func Send(ctx context.Context, filename string, opts Options) error {
	o := opts.defaults()
	if o.Chat {
		if o.Workflow != nil {
			if err := o.Workflow.Validate(); err != nil {
				return err
			}
			if o.Budget == 0 {
				o.Budget = o.Workflow.Budget
			}
		}
		if err := checkGoal(o.Goal, o.Budget); err != nil {
			return err
		}
	}
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Size() > maxFile {
		return errors.New("send requires a regular file no larger than 10 MiB")
	}
	name := filepath.Base(filename)
	if !validName(name) {
		return errors.New("filename is not portable; rename it before sending")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return err
	}
	if len(data) > maxFile {
		return errors.New("file exceeds 10 MiB")
	}
	if looksUnfilled(data) {
		// Sending the blank template happened in practice. It is not an
		// error, since the sender may mean it, but it should not go unnoticed.
		o.Emit(Event{Event: "warning", Message: "the file looks like an unfilled template (many empty \"- field:\" lines); check it is the handoff you meant to send"})
	}
	c, err := code.New()
	if err != nil {
		return err
	}
	blob, digest, err := encrypt(c, name, data, o)
	if err != nil {
		return err
	}
	var chat *chatState
	if o.Chat {
		// The room exists before the packet does, so a receiver can never
		// hold an invitation to a room that is not there.
		if chat, err = createChat(ctx, c, o); err != nil {
			return err
		}
	}
	target, err := endpoint(o.RelayURL, c.Room(), false)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(blob))
	if err != nil {
		return err
	}
	if o.AccessToken != "" {
		req.Header.Set(accessHeader, o.AccessToken)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Handoff-Claim", c.Token("claim"))
	req.Header.Set("X-Handoff-Status", c.Token("status"))
	resp, err := httpClient.Do(req)
	if err != nil {
		chat.abandon(o)
		return fmt.Errorf("cannot reach relay: %w", err)
	}
	reason, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		chat.abandon(o)
		if r := relayReason(reason); r != "" {
			return fmt.Errorf("relay rejected transfer: HTTP %d: %s", resp.StatusCode, r)
		}
		return fmt.Errorf("relay rejected transfer: HTTP %d", resp.StatusCode)
	}
	queued := Event{Event: "queued", Code: c.String(), Relay: o.RelayURL, SHA256: digest, Size: int64(len(data))}
	if chat != nil {
		queued.Chat = chat.ID
	}
	o.Emit(queued)
	if o.WaitTimeout <= 0 {
		return nil
	}
	receiptURL, _ := endpoint(o.RelayURL, c.Room(), true)
	waitCtx, cancel := context.WithTimeout(ctx, o.WaitTimeout)
	defer cancel()
	expected := c.Token("receipt/" + digest + "/" + strconv.Itoa(len(data)))
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		resp, err := request(waitCtx, http.MethodGet, receiptURL, c.Token("status"), nil)
		if err != nil {
			return ErrUnconfirmed
		}
		ack, readErr := io.ReadAll(io.LimitReader(resp.Body, 128))
		resp.Body.Close()
		if readErr != nil {
			return ErrUnconfirmed
		}
		if resp.StatusCode == http.StatusOK {
			if subtle.ConstantTimeCompare(ack, []byte(expected)) != 1 {
				return ErrUnconfirmed
			}
			o.Emit(Event{Event: "delivered", SHA256: digest, Size: int64(len(data))})
			return nil
		}
		if resp.StatusCode != http.StatusNoContent {
			return ErrUnconfirmed
		}
		select {
		case <-waitCtx.Done():
			return ErrUnconfirmed
		case <-ticker.C:
		}
	}
}

func Receive(ctx context.Context, rawCode string, opts Options) (string, error) {
	o := opts.defaults()
	c, err := code.Parse(rawCode)
	if err != nil {
		return "", err
	}
	target, err := endpoint(o.RelayURL, c.Room(), false)
	if err != nil {
		return "", err
	}
	resp, err := request(ctx, http.MethodGet, target, c.Token("claim"), nil)
	if err != nil {
		return "", fmt.Errorf("cannot reach relay: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return "", errors.New("transfer unavailable, expired or already claimed (codes are single-use; ask the sender to send again)")
	}
	blob, err := io.ReadAll(io.LimitReader(resp.Body, maxBlob+1))
	resp.Body.Close()
	if err != nil {
		// The relay released the ciphertext when it answered the claim.
		return "", fmt.Errorf("download interrupted; this code is now used, ask the sender to send again: %w", err)
	}
	if len(blob) > maxBlob {
		return "", errors.New("invalid or oversized encrypted transfer")
	}
	meta, data, err := decrypt(c, blob)
	if err != nil {
		return "", err
	}
	s, err := newSink(o.OutputDir, meta.Filename)
	if err != nil {
		return "", err
	}
	defer s.abort()
	if _, err = s.file.Write(data); err != nil {
		return "", err
	}
	if err = s.commit(); err != nil {
		if s.committed {
			return s.path, fmt.Errorf("file saved but local cleanup failed: %w", ErrUnconfirmed)
		}
		return "", err
	}
	saved := Event{Event: "saved", Path: s.path, SHA256: meta.SHA256, Size: meta.Size, RequiresUserApproval: true}
	if meta.Chat {
		// Only reports that the packet was claimed; joining needs the user.
		reportOpened(ctx, c, o)
		// No chat id here: joining takes the code, and joined returns the id.
		saved.ChatInvite, saved.Goal, saved.Budget = true, meta.Goal, meta.Budget
		// The workflow and the budget the relay actually enforces come from
		// the charter. A charter that fails its checks is reported, not
		// hidden: the user should not join that chat.
		w, terms, err := readCharter(ctx, c, o.RelayURL, c.Token("chat/invite"), meta.Goal)
		switch {
		case errors.Is(err, errNoCharter):
			saved.Workflow = &w // a relay from before workflows: the default applies
		case err != nil:
			saved.Message = "chat invitation failed its checks, do not join: " + err.Error()
		default:
			saved.Workflow = &w
			if terms.Budget > 0 {
				saved.Budget = terms.Budget
			}
		}
	}
	o.Emit(saved)
	receiptURL, _ := endpoint(o.RelayURL, c.Room(), true)
	ack := c.Token("receipt/" + meta.SHA256 + "/" + strconv.FormatInt(meta.Size, 10))
	receipt, err := request(ctx, http.MethodPost, receiptURL, c.Token("claim"), []byte(ack))
	if err != nil {
		return s.path, fmt.Errorf("file saved but receipt failed: %w", ErrUnconfirmed)
	}
	receipt.Body.Close()
	if receipt.StatusCode != http.StatusNoContent {
		return s.path, fmt.Errorf("file saved but receipt failed: %w", ErrUnconfirmed)
	}
	return s.path, nil
}

// looksUnfilled reports a Markdown list of mostly empty "- field:" lines, the
// shape of jand-template.md before anyone writes in it.
func looksUnfilled(data []byte) bool {
	fields, empty := 0, 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		i := strings.IndexAny(line, ":：")
		if i < 0 {
			continue
		}
		fields++
		if strings.TrimSpace(strings.TrimLeft(line[i:], ":：")) == "" {
			empty++
		}
	}
	return empty >= 5 && empty*10 >= fields*6
}

// relayReason turns a relay error body into one short printable line. The
// relay is not trusted, so control characters never reach the terminal.
func relayReason(body []byte) string {
	line := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return ' '
		}
		return r
	}, string(body))
	line = strings.Join(strings.Fields(line), " ")
	if r := []rune(line); len(r) > 120 {
		line = string(r[:120]) + "…"
	}
	return line
}
