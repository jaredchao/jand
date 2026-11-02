package main

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func TestTaskCLIIsLocalAndStrict(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "freebsd" {
		t.Skip("OS lock unavailable; manual mode")
	}
	root := t.TempDir()
	t.Setenv("JAND_HOME", "/does-not-exist")
	t.Setenv("JAND_RELAY", "http://127.0.0.1:1")
	base := `{"op":"init","scope":{"host":"dsh","side":"a","chat":"chat","phase":"p"},"owner":"owner","mode":"two_level"}`
	var out, err bytes.Buffer
	if code := taskCommand([]string{"--root", root}, strings.NewReader(base), &out, &err); code != 0 {
		t.Fatal(code, out.String(), err.String())
	}
	var parsed map[string]any
	if e := json.Unmarshal(out.Bytes(), &parsed); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{base + base, strings.Replace(base, `"op":"init"`, `"op":"init","typo":true`, 1), base + strings.Repeat(" ", 1024*1024)} {
		out.Reset()
		if code := taskCommand([]string{"--root", root}, strings.NewReader(bad), &out, &err); code == 0 {
			t.Fatal("malformed request accepted")
		}
	}
}
