// Package pick chooses a max-iteration count for the codex dispatch loop.
//
// Behavior matches scripts/pick-iterations.sh: fail closed (always return a
// valid integer in [floor, ceiling]), prefer an LLM estimate when available,
// otherwise use a deterministic score based on task length and the number of
// acceptance-criteria lines.
package pick

import (
	"context"
	"strconv"
	"strings"
)

// Options controls how Pick runs.
type Options struct {
	Floor      int
	Ceiling    int
	Model      string
	DisableLLM bool
}

const (
	defaultFloor   = 2
	defaultCeiling = 5
	defaultModel   = "claude-haiku-4-5-20251001"
)

// OptionsFromEnv reads PICK_* env vars via the supplied getenv (typically
// os.Getenv). Invalid integers — including non-positive values — fall back to
// the documented [2,5] defaults so a bad PICK_FLOOR/PICK_CEILING can never
// drive Pick to return 0 or a negative iteration count.
func OptionsFromEnv(getenv func(string) string) Options {
	floor := parsePositiveIntDefault(getenv("PICK_FLOOR"), defaultFloor)
	ceiling := parsePositiveIntDefault(getenv("PICK_CEILING"), defaultCeiling)
	model := getenv("PICK_MODEL")
	if model == "" {
		model = defaultModel
	}
	return Options{
		Floor:      floor,
		Ceiling:    ceiling,
		Model:      model,
		DisableLLM: getenv("PICK_DISABLE_LLM") != "",
	}
}

// parsePositiveIntDefault parses s as an integer, returning def when s is not a
// valid integer or is <= 0. The picker bounds must be positive: a zero or
// negative floor/ceiling would let Pick return a non-positive iteration count.
func parsePositiveIntDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// NormalizeBounds returns (lo, hi) with lo <= hi.
func NormalizeBounds(floor, ceiling int) (int, int) {
	if floor > ceiling {
		return ceiling, floor
	}
	return floor, ceiling
}

// Clamp returns v clamped to [lo, hi]. lo must be <= hi (see NormalizeBounds).
func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Deterministic computes the Bash-equivalent score (caller adds floor).
//
//	score += 1 if len(task) > 100
//	score += 1 if len(task) > 300
//	score += 1 if len(task) > 600
//	score += 1 if non-blank acceptance lines > 1
//	score += 1 if non-blank acceptance lines > 3
func Deterministic(task, acceptance string) int {
	score := 0
	n := len([]rune(task))
	if n > 100 {
		score++
	}
	if n > 300 {
		score++
	}
	if n > 600 {
		score++
	}
	crit := countNonBlankLines(acceptance)
	if crit > 1 {
		score++
	}
	if crit > 3 {
		score++
	}
	return score
}

func countNonBlankLines(s string) int {
	if s == "" {
		return 0
	}
	count := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

// Inputs is what a Runner receives.
type Inputs struct {
	Task       string
	Acceptance string
	Model      string
	Floor      int
	Ceiling    int
}

// Runner runs the LLM-backed pick. Implementations return the raw model
// output (any string); Pick extracts the model's chosen integer via chosenInt
// (the LAST standalone whole-integer token), so preamble prose containing a
// number does not override the final answer.
type Runner interface {
	Run(ctx context.Context, in Inputs) (string, error)
}

// Pick is the top-level entry. It returns a valid integer in
// [normalize(floor), normalize(ceiling)] — never errors, fail-closed.
func Pick(ctx context.Context, task, acceptance string, opts Options, r Runner) int {
	lo, hi := NormalizeBounds(opts.Floor, opts.Ceiling)
	fallback := Clamp(lo+Deterministic(task, acceptance), lo, hi)

	if opts.DisableLLM || r == nil {
		return fallback
	}
	out, err := r.Run(ctx, Inputs{
		Task:       task,
		Acceptance: acceptance,
		Model:      opts.Model,
		Floor:      lo,
		Ceiling:    hi,
	})
	if err != nil {
		return fallback
	}
	if n, ok := chosenInt(out); ok {
		return Clamp(n, lo, hi)
	}
	return fallback
}

// chosenInt extracts the integer the model chose from its reply.
//
// The model is instructed to answer with ONLY a single integer, but real
// replies sometimes carry preamble text ("Based on the task, 4" / "Option 3:
// I'll use 4 iterations"). The chosen answer is the model's final figure, so we
// prefer the LAST standalone integer token rather than the first digit-run
// found anywhere — that earlier rule mis-parsed a digit embedded in preamble
// (e.g. the "3" in "Option 3"). A token counts only when it is a whole
// whitespace-delimited word that is purely digits; this rejects "v2", "3.5",
// and "iter4" so stray identifiers can't masquerade as the answer.
func chosenInt(s string) (int, bool) {
	got := false
	val := 0
	for _, field := range strings.Fields(s) {
		if !allDigits(field) {
			continue
		}
		n, err := strconv.Atoi(field)
		if err != nil {
			continue
		}
		val = n
		got = true
	}
	return val, got
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
