package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestSendHelpShowsCommandUsage(t *testing.T) {
	for _, args := range [][]string{{"send", "--help"}, {"send", "-h"}, {"--json", "--help"}} {
		var out, stderr bytes.Buffer
		if got := run(context.Background(), args, &out, &stderr); got != 0 {
			t.Fatalf("%v: exit %d", args, got)
		}
		if !strings.Contains(out.String(), "jand send [options] <file>") || stderr.Len() != 0 {
			t.Fatalf("%v: out=%q stderr=%q", args, out.String(), stderr.String())
		}
	}
	var out, stderr bytes.Buffer
	if got := run(context.Background(), []string{"send", "--bogus"}, &out, &stderr); got != 2 {
		t.Fatalf("unknown flag: exit %d", got)
	}
	if !strings.Contains(stderr.String(), "jand send [options] <file>") {
		t.Fatalf("unknown flag: stderr=%q", stderr.String())
	}
}

func TestDashLeadingCodeIsNotAFlag(t *testing.T) {
	c := "-aE2VPs842phkNkt3--SBcrnzJHW0xymj1HhNbqvaPI"
	got := protectCodes([]string{"--json", "--relay", "http://x", c})
	if strings.Join(got, " ") != "--json --relay http://x -- "+c {
		t.Fatalf("%q", got)
	}
	// An unknown flag that is not a code stays a usage error.
	if got := protectCodes([]string{"--bogus"}); len(got) != 1 {
		t.Fatalf("%q", got)
	}
}
