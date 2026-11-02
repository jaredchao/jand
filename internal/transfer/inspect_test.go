package transfer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectCoverageAndRelations(t *testing.T) {
	dir := t.TempDir()
	id := "3323ff55ae57c7ea419a68f2e8de5f9c"
	state := `{"id":"` + id + `","relay":"https://user:secret@example.com/?token=secret#secret","role":"host","token":"NEVER_TOKEN","key":"NEVER_KEY","plan":[{"goal":"two","budget":10}],"advance":"auto"}`
	os.WriteFile(filepath.Join(dir, id+".json"), []byte(state), 0600)
	lines := []TranscriptLine{
		{Time: "2026-10-30T01:00:00Z", From: "host", Kind: "started", Text: "one", Budget: 10},
		{Time: "2026-10-30T01:01:00Z", From: "guest", Kind: "message", ID: "g1", Type: "request", Text: "question"},
		{Time: "2026-10-30T01:02:00Z", From: "host", Kind: "message", ID: "h1", Type: "delivery", ReplyTo: "g1", Text: "v1"},
		{Time: "2026-10-30T01:03:00Z", From: "host", Kind: "message", ID: "h2", Type: "delivery", Supersedes: "h1", Text: "v2"},
		{Time: "2026-10-30T01:04:00Z", From: "host", Kind: "done", Text: "done"},
		{Time: "2026-10-30T01:05:00Z", From: "relay", Kind: "checkpoint", Text: "all_done"},
		{Time: "2026-10-30T01:06:00Z", From: "host", Kind: "proposal", Text: "two"},
		{Time: "2026-10-30T01:07:00Z", From: "relay", Kind: "resumed", Text: "two", Budget: 10},
	}
	var data []byte
	for _, l := range lines {
		b, _ := json.Marshal(l)
		data = append(data, append(b, '\n')...)
	}
	tp := filepath.Join(dir, id+".transcript.jsonl")
	os.WriteFile(tp, data, 0600)
	recs := []CostRecord{
		{Time: "2026-10-30T01:00:00Z", Cmd: "start", Out: 100},
		{Time: "2026-10-30T01:01:00Z", Cmd: "recv", Out: 20, Empty: true},
		{Time: "2026-10-30T01:02:00Z", Cmd: "recv", Out: 30, Check: true},
		{Time: "2026-10-30T01:03:00Z", Cmd: "recv", Out: 40, Own: true},
		{Time: "2026-10-30T01:08:00Z", Cmd: "recv", Out: 50, Exit: 4},
	}
	var costs []byte
	for _, r := range recs {
		b, _ := json.Marshal(r)
		costs = append(costs, append(b, '\n')...)
	}
	cp := filepath.Join(dir, id+".cost.jsonl")
	os.WriteFile(cp, costs, 0600)
	d, err := InspectChat(dir, id[:8])
	if err != nil {
		t.Fatal(err)
	}
	if d.SchemaVersion != 1 || d.Chat.Status != "active" || len(d.Sections) != 2 || d.Plan.Proposal != nil || d.Plan.Checkpoint != nil {
		t.Fatalf("bad snapshot: %+v", d)
	}
	if d.Messages[0].Pending || len(d.Messages[0].Answers) != 1 || d.Messages[1].SupersededBy != "h2" || strings.Join(d.Sections[0].Deliveries, ",") != "h2" || len(d.Sections[0].SummaryIndexes) != 1 {
		t.Fatalf("bad relations: %+v", d)
	}
	if d.Cost.Totals.Wakes != 2 || d.Cost.Totals.EmptyWakes != 1 || d.Cost.Totals.OutBytes != 240 || d.Sections[1].Cost.Wakes != 1 {
		t.Fatalf("bad cost: %+v", d.Cost)
	}
	if d.Cost.WallSeconds == nil || *d.Cost.WallSeconds != 480 || *d.Sections[0].WallSeconds != 420 || *d.Sections[1].WallSeconds != 60 {
		t.Fatal("wall time must include waiting through the last observed cost record")
	}
	shown, _ := json.Marshal(d)
	for _, secret := range []string{"NEVER_TOKEN", "NEVER_KEY", "secret"} {
		if strings.Contains(string(shown), secret) {
			t.Fatal("secret in projection")
		}
	}
	before, _ := os.ReadFile(cp)
	InspectChat(dir, id)
	after, _ := os.ReadFile(cp)
	if string(before) != string(after) {
		t.Fatal("inspect wrote costs")
	}
	for _, tc := range []struct {
		name, contents, want string
		totals               bool
	}{
		{"missing", "", "missing", false}, {"empty", "", "empty", false},
		{"partial", string(costs) + `{"time":`, "partial", true},
		{"broken", string(costs) + "{bad}\n", "broken", false},
		{"invalid numbers", `{"time":"2026-10-30T01:00:00Z","cmd":"start","out":-1,"exit":0}` + "\n", "broken", false},
		{"null numbers", `{"time":"2026-10-30T01:00:00Z","cmd":"start","out":null,"exit":0}` + "\n", "broken", false},
		{"overflow", `{"time":"2026-10-30T01:00:00Z","cmd":"start","out":9223372036854775807,"exit":0}` + "\n" + `{"time":"2026-10-30T01:01:00Z","cmd":"recv","out":1,"exit":0}` + "\n", "broken", false},
		{"late", strings.Split(string(costs), "\n")[1] + "\n", "partial", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			os.Remove(cp)
			if tc.name != "missing" {
				os.WriteFile(cp, []byte(tc.contents), 0600)
			}
			got, e := InspectChat(dir, id)
			if e != nil || got.Cost.Integrity.State != tc.want || (got.Cost.Totals != nil) != tc.totals {
				t.Fatalf("%+v %v", got.Cost, e)
			}
		})
	}
	last, _ := json.Marshal(recs[len(recs)-1])
	os.WriteFile(cp, append(last, '\n'), 0600)
	d, err = InspectChat(dir, id)
	if err != nil || d.Sections[0].Cost != nil || d.Sections[0].CostState != "missing" || d.Sections[1].CostState != "partial" {
		t.Fatal("late recording must not show earlier goals as zero cost")
	}
	os.WriteFile(cp, append(append(last, '\n'), []byte(`{"time":`)...), 0600)
	d, err = InspectChat(dir, id)
	if err != nil || d.Cost.Integrity.State != "partial" || d.Sections[0].Cost != nil || d.Sections[0].CostState != "missing" {
		t.Fatal("an unfinished tail must not hide late recording and turn missing history into zero cost")
	}
	os.WriteFile(tp, append(data, []byte("{bad}\n")...), 0600)
	d, err = InspectChat(dir, id)
	if err != nil || d.TranscriptIntegrity.State != "broken" || d.Chat.Status != "unknown" || d.Cost.WallSeconds != nil {
		t.Fatalf("corruption became success: %+v %v", d, err)
	}
}

func TestInspectEveryExplicitReplyToLinksThePeerMessage(t *testing.T) {
	dir := t.TempDir()
	id := "3323ff55ae57c7ea419a68f2e8de5f9c"
	os.WriteFile(filepath.Join(dir, id+".json"), []byte(`{"id":"`+id+`","role":"host"}`), 0600)
	lines := []TranscriptLine{
		{Time: "2026-11-02T01:00:00Z", From: "host", Kind: "started", Text: "goal"},
		{Time: "2026-11-02T01:01:00Z", From: "guest", Kind: "message", ID: "g1", Type: "request", Text: "question"},
		{Time: "2026-11-02T01:02:00Z", From: "host", Kind: "message", ID: "h1", Type: "note", ReplyTo: "g1", Text: "answer in a closing note"},
	}
	var data []byte
	for _, l := range lines {
		b, _ := json.Marshal(l)
		data = append(data, append(b, '\n')...)
	}
	os.WriteFile(filepath.Join(dir, id+".transcript.jsonl"), data, 0600)
	d, err := InspectChat(dir, id)
	if err != nil || d.Messages[0].Pending || strings.Join(d.Messages[0].Answers, ",") != "h1" {
		t.Fatal("protocol allows reply_to on all message kinds, including a closing note")
	}
}
