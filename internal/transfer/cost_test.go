package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jaredchao/jand/internal/code"
)

func TestCostRecord(t *testing.T) {
	dir := t.TempDir()
	id := "3323ff55ae57c7ea419a68f2e8de5f9c"
	if _, err := ChatCost(dir, id); err == nil {
		t.Fatal("no chat, yet a cost record")
	}
	// A chat that does not exist here gets no file, so a decline or a typo
	// leaves nothing behind.
	if err := RecordCost(dir, id, CostRecord{Cmd: "recv", Out: 5}); err == nil {
		t.Fatal("recorded cost for a missing chat")
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*")); len(files) != 0 {
		t.Fatalf("left %v", files)
	}
	os.WriteFile(filepath.Join(dir, id+".json"), []byte(`{"id":"`+id+`","relay":"x","role":"host","token":"t","key":"k"}`), 0600)
	if _, err := ChatCost(dir, id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("chat without a record: %v, want not-exist", err)
	}
	RecordCost(dir, id[:8], CostRecord{Cmd: "start", Out: 300, Wrote: 20})
	RecordCost(dir, id, CostRecord{Cmd: "recv", Out: 60, Empty: true})
	recs, err := ChatCost(dir, id)
	if err != nil || len(recs) != 2 || recs[0].Cmd != "start" || !recs[1].Empty || recs[0].Time == "" {
		t.Fatalf("records %+v, %v", recs, err)
	}
	path := filepath.Join(dir, id+".cost.jsonl")
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	// A damaged line fails the whole record instead of lowering the totals.
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	f.WriteString("{\"cmd\":\"re\n")
	f.Close()
	if _, err := ChatCost(dir, id); err == nil {
		t.Fatal("damaged record read as whole")
	}
}

func TestClaimRecordUsesTheChatID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chats") // not created yet, as on a first receive
	c, _ := code.New()
	if err := RecordClaim(dir, c.String(), CostRecord{Cmd: "claim", Out: 500}); err != nil {
		t.Fatal(err)
	}
	// The record sits where the chat will live once joined, under the id
	// the code derives, and join then appends to it.
	path := filepath.Join(dir, chatRoomID(c)+".cost.jsonl")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("claim record %v %v", info, err)
	}
	if err := RecordClaim(dir, "not-a-code", CostRecord{Cmd: "claim"}); err == nil {
		t.Fatal("claim recorded for a bad code")
	}
	if r := (CostRecord{Cmd: "recv", Check: true}); r.Wake() {
		t.Fatal("recv without --wait counted as a wake")
	}
	if r := (CostRecord{Cmd: "recv", Own: true}); r.Wake() {
		t.Fatal("recv that only saw our own close counted as a wake")
	}
	if r := (CostRecord{Cmd: "recv", Empty: true}); !r.Wake() {
		t.Fatal("a --wait recv that timed out is still a wake")
	}
}
