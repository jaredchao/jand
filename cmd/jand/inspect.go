package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/jaredchao/jand/internal/transfer"
)

func inspectCommand(args []string, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("jand chat inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "versioned JSON snapshot")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if !*jsonOutput || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: jand chat inspect --json <chat>")
		return 2
	}
	d, err := transfer.InspectChat(transfer.DefaultStateDir(), fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := json.NewEncoder(out).Encode(d); err != nil {
		return 1
	}
	return 0
}
