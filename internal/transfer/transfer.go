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
	"time"

	"github.com/jaredchao/jand/internal/code"
)

const (
	version = "handoff/0.2"
	maxFile = 10 * 1024 * 1024
	maxBlob = maxFile + 4096
)

var ErrUnconfirmed = errors.New("delivery unconfirmed: the receiver may have saved the file")

type Event struct {
	Event                string `json:"event"`
	Code                 string `json:"code,omitempty"`
	Path                 string `json:"path,omitempty"`
	SHA256               string `json:"sha256,omitempty"`
	Size                 int64  `json:"size,omitempty"`
	// A fixed receiver policy hint, never evidence that a user approved anything.
	RequiresUserApproval bool   `json:"requires_user_approval,omitempty"`
	Message              string `json:"message,omitempty"`
}

type Options struct {
	RelayURL, OutputDir string
	WaitTimeout         time.Duration // Zero returns as soon as ciphertext is queued.
	Emit                func(Event)
}

func (o Options) defaults() Options {
	if o.OutputDir == "" {
		o.OutputDir = "received"
	}
	if o.Emit == nil {
		o.Emit = func(Event) {}
	}
	return o
}

type metadata struct {
	Protocol string `json:"protocol"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

func endpoint(raw, room string, receipt bool) (string, error) {
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
	u.Path = "/v1/handoffs/" + room
	if receipt {
		u.Path += "/receipt"
	}
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

func encrypt(c code.Code, name string, data []byte) ([]byte, string, error) {
	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])
	meta, err := json.Marshal(metadata{version, name, int64(len(data)), digest})
	if err != nil || len(meta) > 2048 {
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
	if mlen < 1 || mlen > 2048 || mlen > len(plain)-4 {
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
	c, err := code.New()
	if err != nil {
		return err
	}
	blob, digest, err := encrypt(c, name, data)
	if err != nil {
		return err
	}
	target, err := endpoint(o.RelayURL, c.Room(), false)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(blob))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Handoff-Claim", c.Token("claim"))
	req.Header.Set("X-Handoff-Status", c.Token("status"))
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach relay: %w", err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("relay rejected transfer: HTTP %d", resp.StatusCode)
	}
	o.Emit(Event{Event: "queued", Code: c.String(), SHA256: digest, Size: int64(len(data))})
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
		return "", errors.New("transfer unavailable, expired or already claimed")
	}
	blob, err := io.ReadAll(io.LimitReader(resp.Body, maxBlob+1))
	resp.Body.Close()
	if err != nil || len(blob) > maxBlob {
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
	o.Emit(Event{Event: "saved", Path: s.path, SHA256: meta.SHA256, Size: meta.Size, RequiresUserApproval: true})
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
