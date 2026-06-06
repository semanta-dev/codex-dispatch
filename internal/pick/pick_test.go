package pick

import (
	"context"
	"errors"
	"testing"
)

var errBoom = errors.New("boom")

func TestNormalizeBoundsKeepsValid(t *testing.T) {
	lo, hi := NormalizeBounds(2, 5)
	if lo != 2 || hi != 5 {
		t.Fatalf("NormalizeBounds(2,5) = (%d,%d), want (2,5)", lo, hi)
	}
}

func TestNormalizeBoundsSwapsInverted(t *testing.T) {
	lo, hi := NormalizeBounds(5, 2)
	if lo != 2 || hi != 5 {
		t.Fatalf("NormalizeBounds(5,2) = (%d,%d), want (2,5)", lo, hi)
	}
}

func TestClamp(t *testing.T) {
	cases := []struct {
		v, lo, hi, want int
	}{
		{3, 2, 5, 3},
		{1, 2, 5, 2},
		{100, 2, 5, 5},
		{2, 2, 5, 2},
		{5, 2, 5, 5},
	}
	for _, c := range cases {
		if got := Clamp(c.v, c.lo, c.hi); got != c.want {
			t.Errorf("Clamp(%d,%d,%d) = %d, want %d", c.v, c.lo, c.hi, got, c.want)
		}
	}
}

func TestDeterministicEmptyTask(t *testing.T) {
	if got := Deterministic("", ""); got != 0 {
		t.Fatalf("Deterministic(\"\",\"\") = %d, want 0", got)
	}
}

func TestDeterministicMonotonicByLength(t *testing.T) {
	short := Deterministic("x", "")
	long := Deterministic(strRepeat("x", 1000), "")
	if long < short {
		t.Fatalf("longer task should score >= shorter (long=%d short=%d)", long, short)
	}
}

func TestDeterministicMonotonicByAcceptance(t *testing.T) {
	few := Deterministic("task", "")
	many := Deterministic("task", "a\nb\nc\nd\ne")
	if many < few {
		t.Fatalf("more criteria should score >= fewer (many=%d few=%d)", many, few)
	}
}

func TestDeterministicBoundaryScores(t *testing.T) {
	if got := Deterministic(strRepeat("x", 50), ""); got != 0 {
		t.Fatalf("short task score = %d, want 0", got)
	}
	got := Deterministic(strRepeat("x", 700), "a\nb\nc\nd")
	if got != 5 {
		t.Fatalf("long task with many criteria score = %d, want 5", got)
	}
}

func TestOptionsFromEnvDefaults(t *testing.T) {
	opts := OptionsFromEnv(func(string) string { return "" })
	if opts.Floor != 2 || opts.Ceiling != 5 {
		t.Errorf("defaults = (%d,%d), want (2,5)", opts.Floor, opts.Ceiling)
	}
	if opts.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("default model = %q", opts.Model)
	}
	if opts.DisableLLM {
		t.Errorf("DisableLLM should default to false")
	}
}

func TestOptionsFromEnvInvalidBoundsFallToDefaults(t *testing.T) {
	env := map[string]string{"PICK_FLOOR": "abc", "PICK_CEILING": "-"}
	opts := OptionsFromEnv(func(k string) string { return env[k] })
	if opts.Floor != 2 || opts.Ceiling != 5 {
		t.Fatalf("invalid bounds gave (%d,%d), want defaults (2,5)", opts.Floor, opts.Ceiling)
	}
}

func TestOptionsFromEnvRejectsNonPositiveBounds(t *testing.T) {
	cases := []struct {
		name           string
		floor, ceiling string
		wantLo, wantHi int
	}{
		{"zero-floor", "0", "5", defaultFloor, 5},
		{"negative-floor", "-3", "5", defaultFloor, 5},
		{"zero-ceiling", "2", "0", 2, defaultCeiling},
		{"negative-ceiling", "2", "-1", 2, defaultCeiling},
		{"both-non-positive", "0", "0", defaultFloor, defaultCeiling},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{"PICK_FLOOR": c.floor, "PICK_CEILING": c.ceiling}
			opts := OptionsFromEnv(func(k string) string { return env[k] })
			if opts.Floor != c.wantLo || opts.Ceiling != c.wantHi {
				t.Fatalf("OptionsFromEnv(floor=%q,ceiling=%q) = (%d,%d), want (%d,%d)",
					c.floor, c.ceiling, opts.Floor, opts.Ceiling, c.wantLo, c.wantHi)
			}
		})
	}
}

func TestOptionsFromEnvDisableLLM(t *testing.T) {
	env := map[string]string{"PICK_DISABLE_LLM": "1"}
	opts := OptionsFromEnv(func(k string) string { return env[k] })
	if !opts.DisableLLM {
		t.Fatalf("PICK_DISABLE_LLM=1 should set DisableLLM")
	}
}

func strRepeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// fakeRunner records calls and returns a canned response.
type fakeRunner struct {
	out string
	err error
	got Inputs
}

func (f *fakeRunner) Run(ctx context.Context, in Inputs) (string, error) {
	f.got = in
	return f.out, f.err
}

func TestPickUsesLLMWhenValid(t *testing.T) {
	r := &fakeRunner{out: "3\n"}
	got := Pick(context.Background(), "task", "criteria", Options{Floor: 2, Ceiling: 5, DisableLLM: false}, r)
	if got != 3 {
		t.Fatalf("Pick = %d, want 3", got)
	}
}

func TestPickClampsLLMAboveCeiling(t *testing.T) {
	r := &fakeRunner{out: "100"}
	got := Pick(context.Background(), "task", "", Options{Floor: 2, Ceiling: 5}, r)
	if got != 5 {
		t.Fatalf("Pick = %d, want 5 (clamped)", got)
	}
}

func TestPickClampsLLMBelowFloor(t *testing.T) {
	r := &fakeRunner{out: "0"}
	got := Pick(context.Background(), "task", "", Options{Floor: 2, Ceiling: 5}, r)
	if got != 2 {
		t.Fatalf("Pick = %d, want 2 (clamped)", got)
	}
}

func TestPickLLMNoIntegerFallsBackDeterministic(t *testing.T) {
	r := &fakeRunner{out: "I do not know."}
	got := Pick(context.Background(), "task", "", Options{Floor: 2, Ceiling: 5}, r)
	if got < 2 || got > 5 {
		t.Fatalf("Pick = %d, want in [2,5]", got)
	}
}

func TestPickLLMErrorFallsBackDeterministic(t *testing.T) {
	r := &fakeRunner{err: errBoom}
	got := Pick(context.Background(), "task", "", Options{Floor: 2, Ceiling: 5}, r)
	if got < 2 || got > 5 {
		t.Fatalf("Pick = %d, want in [2,5]", got)
	}
}

func TestPickDisableLLMSkipsRunner(t *testing.T) {
	r := &fakeRunner{out: "4"}
	got := Pick(context.Background(), "task", "", Options{Floor: 2, Ceiling: 2, DisableLLM: true}, r)
	if got != 2 {
		t.Fatalf("Pick (DisableLLM) = %d, want 2", got)
	}
	if r.got.Task != "" || r.got.Acceptance != "" {
		t.Fatalf("Runner should not have been called: got = %+v", r.got)
	}
}

func TestPickNilRunnerUsesDeterministic(t *testing.T) {
	got := Pick(context.Background(), "task", "", Options{Floor: 2, Ceiling: 5}, nil)
	if got < 2 || got > 5 {
		t.Fatalf("Pick (nil runner) = %d, want in [2,5]", got)
	}
}

func TestPickInvertedBoundsNormalize(t *testing.T) {
	got := Pick(context.Background(), "task", "", Options{Floor: 5, Ceiling: 2, DisableLLM: true}, nil)
	if got < 2 || got > 5 {
		t.Fatalf("Pick (inverted bounds) = %d, want in [2,5]", got)
	}
}
