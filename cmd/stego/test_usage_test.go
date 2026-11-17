package main

import (
	"strings"
	"testing"
)

func TestStegoTestFlags(t *testing.T) {
	t.Run("ST-EXT-UC02-R01-T05 stego test takes its go test flags before or after --, and -h prints its usage", func(t *testing.T) {
		project := declaringProject(t)
		help := stegoIn(t, project, nil, "test", "-h")
		if help.code != 0 || !strings.Contains(help.stderr+help.stdout, "Usage: stego test [--] [go test flags]") || !strings.Contains(help.stderr+help.stdout, "-count n") {
			t.Fatalf("stego test -h: exit %d, want 0 and the usage with the passed flags:\n%s%s", help.code, help.stdout, help.stderr)
		}
		// A flag before -- is a flag of go test, held to the same rules as after --: -count=0 runs no test.
		for _, args := range [][]string{{"test", "-count=0"}, {"test", "--", "-count=0"}} {
			run := stegoIn(t, project, nil, args...)
			if run.code == 0 || !strings.Contains(run.stderr, "the count must be a whole number of 1 or more") {
				t.Errorf("stego %s: exit %d, want the refusal of the count:\n%s%s", strings.Join(args, " "), run.code, run.stdout, run.stderr)
			}
			if strings.Contains(run.stderr, "flag provided but not defined") {
				t.Errorf("stego %s gave the flag parse error of stego test, not the rule of go test flags:\n%s", strings.Join(args, " "), run.stderr)
			}
		}
		if run := stegoIn(t, project, nil, "test", "-run=X"); run.code == 0 || !strings.Contains(run.stderr, `does not pass "-run=X"`) {
			t.Errorf("stego test -run=X: exit %d, want the refusal that names -run=X:\n%s", run.code, run.stderr)
		}
	})
}
