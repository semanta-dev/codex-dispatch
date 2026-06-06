package pick

import (
	"context"
	"testing"
)

// stubRunner is a minimal pick.Runner that returns canned output/err. It lets
// these tests drive Pick's LLM-output parsing (the chosenInt path) without
// shelling out to the real `claude` CLI that ClaudeRunner wraps.
type stubRunner struct {
	out string
	err error
}

func (s stubRunner) Run(context.Context, Inputs) (string, error) { return s.out, s.err }

// TestPickParsesCleanInteger covers the happy path: the model obeys the prompt
// and replies with ONLY an integer (with trailing newline/whitespace).
func TestPickParsesCleanInteger(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want int
	}{
		{"bare", "3", 3},
		{"trailing-newline", "3\n", 3},
		{"surrounding-space", "  4  \n", 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Pick(context.Background(), "task", "crit", Options{Floor: 2, Ceiling: 5}, stubRunner{out: c.out})
			if got != c.want {
				t.Fatalf("Pick(%q) = %d, want %d", c.out, got, c.want)
			}
		})
	}
}

// TestPickIgnoresPreambleDigit is the regression for the old firstInt behavior,
// which returned the FIRST digit-run anywhere in the reply. With preamble text
// that itself contains a number, the chosen answer is the model's final figure,
// not a digit embedded in the prose ("Option 3" / "step 2").
func TestPickIgnoresPreambleDigit(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want int
	}{
		{"option-prefix", "Option 3: I'll use 4 iterations\n4", 4},
		{"sentence-then-answer", "Based on the task complexity, I'd say 5", 5},
		{"step-preamble", "step 2 of analysis -> 3", 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Pick(context.Background(), "task", "crit", Options{Floor: 2, Ceiling: 5}, stubRunner{out: c.out})
			if got != c.want {
				t.Fatalf("Pick(%q) = %d, want %d (chosen integer, not preamble digit)", c.out, got, c.want)
			}
		})
	}
}

// TestChosenIntRejectsEmbeddedDigits asserts chosenInt only accepts whole
// numeric tokens, so identifiers like "v2", "3.5" or "iter4" never count as the
// model's answer.
func TestChosenIntRejectsEmbeddedDigits(t *testing.T) {
	for _, out := range []string{"v2", "3.5", "iter4", "use-2x"} {
		if n, ok := chosenInt(out); ok {
			t.Fatalf("chosenInt(%q) = %d, ok=true; want no whole-integer token", out, n)
		}
	}
}

// TestPickEmptyOrGarbageFallsBack: an empty reply or one with no integer token
// must fall back to the deterministic estimate clamped into [floor,ceiling].
func TestPickEmptyOrGarbageFallsBack(t *testing.T) {
	for _, out := range []string{"", "   \n  ", "no idea", "version v2 only"} {
		got := Pick(context.Background(), "task", "", Options{Floor: 2, Ceiling: 5}, stubRunner{out: out})
		if got < 2 || got > 5 {
			t.Fatalf("Pick(%q) = %d, want deterministic fallback in [2,5]", out, got)
		}
	}
}

// TestPickRunnerErrorFallsBack: a non-zero-exit / failed runner call (the real
// ClaudeRunner returns an error from cmd.Run on non-zero exit) must yield the
// deterministic fallback, never an error.
func TestPickRunnerErrorFallsBack(t *testing.T) {
	got := Pick(context.Background(), "task", "", Options{Floor: 2, Ceiling: 5}, stubRunner{err: errBoom})
	if got < 2 || got > 5 {
		t.Fatalf("Pick(err) = %d, want deterministic fallback in [2,5]", got)
	}
}

// TestPickClampsParsedAnswer: a parsed answer outside the bounds is clamped, so
// even a well-formed but out-of-range model reply honors [floor,ceiling].
func TestPickClampsParsedAnswer(t *testing.T) {
	if got := Pick(context.Background(), "t", "", Options{Floor: 2, Ceiling: 5}, stubRunner{out: "the answer is 99"}); got != 5 {
		t.Fatalf("over-ceiling answer = %d, want 5 (clamped)", got)
	}
	if got := Pick(context.Background(), "t", "", Options{Floor: 2, Ceiling: 5}, stubRunner{out: "I pick 0"}); got != 2 {
		t.Fatalf("below-floor answer = %d, want 2 (clamped)", got)
	}
}
