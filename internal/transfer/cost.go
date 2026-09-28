package transfer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CostRecord is what one jand command cost this side of a chat (see
// docs/CORE.md): the bytes it handed the agent on stdout and stderr, the
// bytes of text the agent sent through it, and whether it was a recv that
// found nothing. It holds numbers and the subcommand only, never text.
type CostRecord struct {
	Time  string `json:"time"`
	Cmd   string `json:"cmd"`
	Out   int64  `json:"out"`
	Wrote int64  `json:"wrote,omitempty"`
	Empty bool   `json:"empty,omitempty"`
	// Check marks a recv without --wait: the agent looked on its own, it was
	// not woken. Own marks a recv that printed only this side's own action
	// (a close it had just made): nobody else woke the agent either.
	Check bool `json:"check,omitempty"`
	Own   bool `json:"own,omitempty"`
	Exit  int  `json:"exit"`
}

// Wake says whether the record is a recv that woke the agent for the peer.
func (r CostRecord) Wake() bool { return r.Cmd == "recv" && !r.Check && !r.Own }

// RecordCost appends r to the chat's cost record, stamped with the current
// time. A chat with no local state (declined, or a mistyped id) gets no
// record, so nothing is left behind for a chat that does not exist here.
func RecordCost(dir, id string, r CostRecord) error {
	s, err := loadChat(dir, id)
	if err != nil {
		return err
	}
	return appendCost(s.path(dir, ".cost.jsonl"), r)
}

// RecordClaim records receiving a chat invite. The chat has no local state
// until the user agrees to join, so the record is keyed by the id the code
// derives; declining removes it again (see ChatDecline).
func RecordClaim(dir, rawCode string, r CostRecord) error {
	c, err := parseInviteCode(rawCode)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return appendCost(filepath.Join(dir, chatRoomID(c)+".cost.jsonl"), r)
}

func appendCost(path string, r CostRecord) error {
	r.Time = time.Now().UTC().Format(time.RFC3339)
	line, _ := json.Marshal(r)
	// The background recv and a foreground send may both append: one write
	// of one line to a file opened for appending keeps the lines whole.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ChatCost reads a chat's cost record. A chat from before 0.4.4 has none,
// and the error then satisfies errors.Is(err, os.ErrNotExist). A line that
// cannot be read fails the whole record: a total that silently skipped it
// would look complete and be too low.
func ChatCost(dir, id string) ([]CostRecord, error) {
	s, err := loadChat(dir, id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(s.path(dir, ".cost.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var recs []CostRecord
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		var r CostRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil || r.Cmd == "" {
			return nil, fmt.Errorf("cost record line %d is unreadable", n)
		}
		recs = append(recs, r)
	}
	return recs, sc.Err()
}
