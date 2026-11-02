package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"runtime"

	"github.com/jaredchao/jand/internal/taskcontrol"
)

// taskCommand is local-only and never loads Relay credentials or pages.
func taskCommand(args []string, in io.Reader, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("task", flag.ContinueOnError)
	fs.SetOutput(stderr)
	capabilities := fs.Bool("capabilities", false, "read task contract and platform support without opening state")
	root := fs.String("root", "", "absolute task ledger directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return 2
	}
	if *capabilities {
		json.NewEncoder(out).Encode(map[string]any{"contract": "jand-task-v1", "schema": 1, "locking": taskcontrol.Supported(), "platform": runtime.GOOS})
		return 0
	}
	var q taskcontrol.Request
	data, readErr := io.ReadAll(io.LimitReader(in, 1024*1024+1))
	if readErr != nil || len(data) > 1024*1024 {
		json.NewEncoder(out).Encode(map[string]string{"outcome": "error", "error": "request unreadable or exceeds 1 MiB"})
		return 1
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&q)
	if err == nil {
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			err = fmt.Errorf("exactly one request required")
		}
	}
	var r taskcontrol.Response
	if err == nil {
		r, err = taskcontrol.Apply(*root, q)
	}
	if err != nil {
		json.NewEncoder(out).Encode(map[string]string{"outcome": "error", "error": err.Error()})
		return 1
	}
	if err = json.NewEncoder(out).Encode(r); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
