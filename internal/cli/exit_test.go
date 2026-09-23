package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestExitCodeSeparatesUsageFromFailure. Every error used to leave the
// process with status 1, so a script could not tell "I typed the command
// wrong" from "the call ran and failed" — and only the first of those is
// worth stopping and fixing rather than retrying.
func TestExitCodeSeparatesUsageFromFailure(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"unknown flag", []string{"events", "list", "--nope"}, ExitUsage},
		{"too few arguments", []string{"api", "GET"}, ExitUsage},
		{"too many arguments", []string{"events", "retrieve", "evt_1", "evt_2"}, ExitUsage},
		{"a bad flag value", []string{"events", "list", "--limit", "banana"}, ExitUsage},
		// Ran fine as an invocation; the call itself could not be made.
		{"no credentials", []string{"events", "list", "--base-url", "https://api.example.test"}, ExitFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
			t.Setenv("BILLKIT_API_KEY", "")

			root := rootCmd()
			root.SetOut(io.Discard)
			root.SetErr(&bytes.Buffer{})
			root.SetIn(strings.NewReader(""))
			root.SetArgs(tc.args)

			err := root.Execute()
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := ExitCode(err); got != tc.want {
				t.Fatalf("ExitCode = %d, want %d (err: %v)", got, tc.want, err)
			}
		})
	}
}

func TestExitCodeOfNilIsZero(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Fatalf("ExitCode(nil) = %d, want 0", got)
	}
}
