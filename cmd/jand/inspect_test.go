package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectCLIHasNoSideEffects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JAND_HOME", home)
	// An invalid client config catches accidental page/config or network work.
	os.WriteFile(filepath.Join(home, "config.json"), []byte("broken"), 0600)
	dir := filepath.Join(home, "chats")
	os.Mkdir(dir, 0700)
	id := "3323ff55ae57c7ea419a68f2e8de5f9c"
	os.WriteFile(filepath.Join(dir, id+".json"), []byte(`{"id":"`+id+`","role":"host","relay":"http://127.0.0.1:1","token":"t","key":"k"}`), 0600)
	var out, stderr bytes.Buffer
	if c := run(context.Background(), []string{"chat", "inspect", "--json", id}, &out, &stderr); c != 0 {
		t.Fatalf("exit %d: %s", c, stderr.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(`"schema_version":1`)) {
		t.Fatal(out.String())
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatal("inspect wrote files")
	}
}
