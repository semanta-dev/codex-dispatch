#!/usr/bin/env bash
# tests/sdk/compare-headless.sh — head-to-head: direct Claude vs codex-dispatch
# plugin, driven by the real Claude Code CLI (`claude --print`) rather than
# the SDK. This is the production code path users actually invoke.
#
# Per task we run two scenarios in throwaway repos:
#   direct  — `claude --print "<task>"` with full toolset (Read/Write/Edit/Bash).
#             One Claude session; Claude does the work itself.
#   plugin  — `claude --print --plugin-dir $REPO "/codex-dispatch:codex ..."`
#             Claude invokes the slash command, which dispatches real codex
#             through the per-cwd broker.
#
# Captures per leg:
#   - Anthropic usage + total_cost_usd from the `result` stream-json message.
#   - Codex usage (input/output/reasoning tokens, last turn duration) from
#     the plugin run's .codex-dispatch/runs/<ts>-<pid>/stdout.log.
#   - Wall-clock from /usr/bin/time wrapping the claude invocation.
#   - Task verifier verdict.
#
# Auth follows run-fixtures.sh: ANTHROPIC_API_KEY users get --bare; everyone
# else relies on Claude Code's OAuth keychain.
#
# Env:
#   COMPARE_MODE             easy (default) | trivial-only
#   COMPARE_MODEL            claude-sonnet-4-6 (default)
#   COMPARE_MAX_BUDGET_USD   2.00 per leg (default)
#   COMPARE_MAX_ITER         1 (default)
#   CODEX_DISPATCH_BIN       override binary path; auto-build if unset
#   CODEX_SANDBOX            danger-full-access (default — hosts without
#                            unprivileged bubblewrap can't workspace-write)
#   COMPARE_DRY_RUN          1: print the plan and skip API calls
#
# Output: human-readable table to stdout plus a results.json next to the script.

set -euo pipefail

err()  { printf 'compare-headless: %s\n' "$*" >&2; }
note() { printf '  %s\n' "$*"; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODEL="${COMPARE_MODEL:-claude-sonnet-4-6}"
BUDGET="${COMPARE_MAX_BUDGET_USD:-2.00}"
MAX_ITER="${COMPARE_MAX_ITER:-1}"
MODE="${COMPARE_MODE:-easy}"
DRY_RUN="${COMPARE_DRY_RUN:-0}"
SANDBOX="${CODEX_SANDBOX:-danger-full-access}"

if ! command -v claude >/dev/null 2>&1; then
  err "claude CLI not on PATH"; exit 3
fi
if ! command -v codex >/dev/null 2>&1; then
  err "codex CLI not on PATH (the plugin leg needs it)"; exit 3
fi

# Ensure CODEX_DISPATCH_BIN: build the local binary if not already set.
if [ -z "${CODEX_DISPATCH_BIN:-}" ] || [ ! -x "${CODEX_DISPATCH_BIN:-/nonexistent}" ]; then
  err "building local codex-dispatch binary (CODEX_DISPATCH_BIN unset)"
  ( cd "$REPO_ROOT" && go build -o ./dist/codex-dispatch ./cmd/codex-dispatch ) >&2
  export CODEX_DISPATCH_BIN="$REPO_ROOT/dist/codex-dispatch"
fi

# Auth follows run-fixtures.sh.
bare_flag=""
[ -n "${ANTHROPIC_API_KEY:-}" ] && bare_flag="--bare"

RESULTS_DIR="$(mktemp -d)"
RESULTS_JSON="${RESULTS_JSON:-$REPO_ROOT/tests/sdk/results-headless.json}"
trap 'rm -rf "$RESULTS_DIR"' EXIT

# ---------------------------------------------------------------------------
# Task definitions. Each task is a function `task_<name>` that, given a leg
# directory, runs the verifier and prints PASS or FAIL: <reason>.
# Prompts and acceptance lines are in the maps below.
# ---------------------------------------------------------------------------

declare -A PROMPTS ACCEPTANCE
declare -a TASK_ORDER

add_task() {
  local name="$1" prompt="$2" acc="$3"
  TASK_ORDER+=("$name")
  PROMPTS[$name]="$prompt"
  ACCEPTANCE[$name]="$acc"
}

add_task trivial \
  "Create a file hello.txt with the exact content 'Hello, World!' (single line, no extra whitespace)." \
  "hello.txt MUST be exactly the 13 bytes 'Hello, World!' (no trailing newline, no leading whitespace, no extra characters); file size equals 13 bytes"

add_task small \
  "Add a Python module src/strings.py with a function reverse_string(s) that returns the reversed string. Pure function, no side effects." \
  "src/strings.py exists; reverse_string handles empty string, single char, palindrome, embedded whitespace and newlines, unicode (Latin-1), and does not mutate its argument"

add_task medium \
  "Implement a Stack class in src/stack.py with these methods: push(value), pop() (returns top, raises IndexError if empty), peek() (returns top without removing), size() (returns count), is_empty() (returns bool). Standard LIFO semantics." \
  "src/stack.py exists; push/pop/peek/size/is_empty work for empty, single, and multi-element stacks; pop on empty raises IndexError SPECIFICALLY (not generic Exception); peek does not modify state; None is a valid value"

add_task csv-parser \
  "Implement parse_csv(text) in src/csvparser.py. Returns a list of rows; each row is a list of field strings. Must correctly handle: simple comma-separated values; double-quoted fields; commas inside quoted fields; literal newlines inside quoted fields; escaped double-quote inside a quoted field (the RFC-4180 \"\"  pattern, two double-quotes representing one); trailing newline at end of input (should not produce an extra empty row); empty fields. Pure function, stdlib only." \
  "src/csvparser.py exists; parses LF and CRLF line endings; trailing newline does not produce empty row; empty input returns [] or [[\"\"]]; escaped quotes; embedded commas and newlines inside quoted fields; empty fields; unicode round-trips; multi-row long inputs"

add_task lru-cache \
  "Implement LRUCache in src/lru.py with __init__(capacity), get(key), put(key, value). get returns the value or -1 if absent and counts as a recency touch. put inserts or updates; if at capacity and inserting a new key, evict the least-recently-used key. Updating an existing key with put must not evict anything. Both get and put must be O(1) average." \
  "src/lru.py exists; capacity=0 and capacity=1 cases work; get is a recency touch; put-on-existing-key is also a recency touch and does NOT evict; None is a valid value distinguishable from missing (which returns -1); long workloads evict the correct LRU key, not FIFO"

add_task merge-intervals \
  "Implement merge_intervals(intervals) in src/intervals.py. intervals is a list of (start, end) tuples where each is an ISO date string YYYY-MM-DD. Endpoints are inclusive. Two intervals merge if they overlap OR are adjacent (one ends the day before the other starts). Result must be sorted by start date and contain non-overlapping, non-adjacent intervals. Handle empty input. Do not mutate the argument." \
  "src/intervals.py exists; correctly handles empty input, single interval, disjoint, overlapping, fully-contained, and adjacent (one ends day before next starts) cases; handles month / year / LEAP-YEAR boundaries (Feb 29 2024 → Mar 1 2024 is adjacent); single-day intervals on consecutive days merge; result is sorted; argument is not mutated"

# Hard set — long-form design-doc tasks where the plugin's iterate-and-review
# loop has room to add value. MAX_ITER>=3 recommended.

declare -a HARD_TASK_ORDER
add_hard_task() {
  local name="$1" prompt="$2" acc="$3"
  HARD_TASK_ORDER+=("$name")
  PROMPTS[$name]="$prompt"
  ACCEPTANCE[$name]="$acc"
}

add_hard_task rate-limiter \
  "Write \`docs/rate-limiter.md\`, a complete design specification for a distributed rate-limiter service. Cover: goals & non-goals; core invariants; data model and storage choice; the algorithm (compare token bucket and sliding window — discuss precision, burstiness, memory cost; pick one with justification); the public API surface (key shapes, response shapes, error semantics); how to handle distributed coordination (clock skew, race conditions, monotonic vs wall clock); failure modes and degradation strategy; observability (metrics + key dashboards); operational concerns (warm-up, capacity sizing); open questions. Sustained design thinking, not code. Target 1500-3000 words." \
  "docs/rate-limiter.md exists; covers goals/non-goals, data model, algorithm choice with justification, API surface, distributed coordination, failure modes, observability, operations, open questions; 1500-3000 words"

add_hard_task pagination \
  "Write \`docs/pagination.md\`, a thorough spec for the pagination strategy of a public REST list-API serving millions of users. Compare offset-based vs cursor-based vs keyset pagination explicitly with their tradeoffs (ordering guarantees, drift on insert/delete, deep-page performance, total-count semantics, caching friendliness). Pick one and justify. Then specify: the request/response shape, sort-stability requirements, how to handle filtering combined with pagination, edge cases (empty results, item deleted between pages, item inserted between pages), and migration strategy from a hypothetical existing offset-based system. 1200-2500 words." \
  "docs/pagination.md exists; compares offset/cursor/keyset with tradeoffs; picks one with justification; specifies request/response shape, sort stability, filtering, edge cases, migration; 1200-2500 words"

add_hard_task feature-flags \
  "Write \`docs/feature-flags.md\`, the design for an internal feature-flag service. Cover: motivation; flag lifecycle (creation, ramp, gradual rollout, cleanup); evaluation model (boolean / multivariate / percentage / rule-based / targeted); architecture (where evaluation runs — server, client SDK, edge); consistency vs latency tradeoff (strict freshness vs eventually-consistent SDK cache); audit and access control; circuit-breaker / kill-switch story; observability (which flag changed, who saw what variation, exposure metrics); dependencies between flags; testing strategy; rollback. 1500-3000 words." \
  "docs/feature-flags.md exists; covers motivation, lifecycle, evaluation model, architecture, consistency tradeoffs, audit/ACL, kill-switch, observability, dependencies, testing, rollback; 1500-3000 words"

# ---------------------------------------------------------------------------
# Verifiers — graded edge-case batteries.
#
# Each easy-set verifier delegates to tests/sdk/quality/<task>.py which runs
# 10+ assertions and prints "SCORE: P/N" on the final line plus failed-case
# names. The bash wrapper turns that into a verdict string like
# "PASS 12/12", "PARTIAL 8/12: case_a;case_b;...", or "FAIL 0/12: ...".
#
# Hard-set verifiers below stay rubric-based (sections + concepts + word count).
# ---------------------------------------------------------------------------

verdict_from_score() {
  local task="$1"
  local quality="$REPO_ROOT/tests/sdk/quality/${task}.py"
  if [ ! -f "$quality" ]; then
    echo "FAIL: no quality test file at $quality"; return
  fi
  local out
  # The graded test now exits 1 on imperfect scores (so it's usable as
  # --test-cmd for the plugin's reviewer). Absorb the exit code here; we
  # parse SCORE: P/N from stdout regardless.
  out="$(python3 "$quality" 2>&1 || true)"
  local score
  score="$(printf '%s\n' "$out" | grep -E '^SCORE: [0-9]+/[0-9]+$' | tail -1)"
  if [ -z "$score" ]; then
    local reason
    reason="$(printf '%s\n' "$out" | tail -1 | cut -c1-120)"
    echo "FAIL 0/?: $reason"
    return
  fi
  local p n
  p="$(echo "$score" | awk '{print $2}' | cut -d/ -f1)"
  n="$(echo "$score" | awk '{print $2}' | cut -d/ -f2)"
  local fails_summary
  fails_summary="$(printf '%s\n' "$out" | grep -E '^  - ' | head -3 | sed 's/^  - //' | paste -sd';' | cut -c1-160)"
  if [ "$p" = "$n" ]; then
    echo "PASS $p/$n"
  elif [ "$p" = "0" ]; then
    echo "FAIL 0/$n: $fails_summary"
  else
    echo "PARTIAL $p/$n: $fails_summary"
  fi
}

verify_trivial()    { verdict_from_score trivial; }
verify_small()      { verdict_from_score small; }
verify_medium()     { verdict_from_score medium; }
verify_csv-parser() { verdict_from_score csv-parser; }
verify_lru-cache()  { verdict_from_score lru-cache; }

# Hard verifiers — rubric checks for design docs. Each looks for required
# section headings + key concept words (case-insensitive) and a word count
# in range. Coverage threshold: at least 80% of required sections and 70%
# of required concepts. Failures list what was missing.

verify_doc_rubric() {
  local doc_path="$1"; shift
  local min_words="$1"; shift
  local max_words="$1"; shift
  local sections_csv="$1"; shift
  local concepts_csv="$1"; shift
  python3 - <<PYEOF
import os, re, sys
path = "$doc_path"
if not os.path.exists(path):
    print(f"FAIL: missing {path}")
    sys.exit(0)
text = open(path).read()
wc = len(text.split())
if wc < $min_words:
    print(f"FAIL: too short ({wc} words, need >= $min_words)")
    sys.exit(0)
if wc > $max_words * 2:
    # Hard ceiling at 2x the target — past this we suspect the doc rambled.
    print(f"FAIL: too long ({wc} words, soft target <= $max_words)")
    sys.exit(0)
sections = [s.strip() for s in "$sections_csv".split(",") if s.strip()]
concepts = [c.strip() for c in "$concepts_csv".split(",") if c.strip()]
low = text.lower()
missing_sec = [s for s in sections if s.lower() not in low]
missing_con = [c for c in concepts if c.lower() not in low]
sec_cov = 1 - len(missing_sec) / max(1, len(sections))
con_cov = 1 - len(missing_con) / max(1, len(concepts))
if sec_cov < 0.80:
    print(f"FAIL: section coverage {sec_cov:.0%}; missing: {missing_sec[:4]}")
    sys.exit(0)
if con_cov < 0.70:
    print(f"FAIL: concept coverage {con_cov:.0%}; missing: {missing_con[:4]}")
    sys.exit(0)
print(f"PASS ({wc} words, sections {sec_cov:.0%}, concepts {con_cov:.0%})")
PYEOF
}

verify_rate-limiter() {
  verify_doc_rubric "docs/rate-limiter.md" 1500 3000 \
    "goals,non-goals,data model,algorithm,API,coordination,failure,observability,open questions" \
    "token bucket,sliding window,clock skew,race condition,burstiness,degradation,metrics"
}

verify_pagination() {
  verify_doc_rubric "docs/pagination.md" 1200 2500 \
    "request,response,sort,filtering,edge cases,migration" \
    "offset,cursor,keyset,ordering,deep page,drift,total count,caching"
}

verify_feature-flags() {
  verify_doc_rubric "docs/feature-flags.md" 1500 3000 \
    "motivation,lifecycle,evaluation,architecture,consistency,audit,kill-switch,observability,dependencies,testing,rollback" \
    "boolean,multivariate,percentage,rule-based,targeted,SDK,cache,exposure"
}

verify_merge-intervals() { verdict_from_score merge-intervals; }

# ---------------------------------------------------------------------------
# Per-leg runners.
# ---------------------------------------------------------------------------

# extract_claude_metrics <stream-json-file>
# prints exactly one line: "cost_usd duration_ms turns is_success". Always
# exits 0 so callers don't need a `|| echo` fallback (which doubled the
# output under pipefail when the stream file was empty/missing).
extract_claude_metrics() {
  local f="$1"
  if [ ! -s "$f" ]; then
    echo "0.0 0 0 0"
    return 0
  fi
  python3 -c "
import sys, json
result = None
with open('$f') as fh:
    for line in fh:
        try:
            m = json.loads(line)
        except Exception:
            continue
        if m.get('type') == 'result':
            result = m
            break
if not result:
    print('0.0 0 0 0')
else:
    cost = result.get('total_cost_usd', 0.0)
    dur = result.get('duration_ms', 0)
    turns = result.get('num_turns', 0)
    success = 1 if result.get('subtype') == 'success' else 0
    print(f'{cost:.4f} {dur} {turns} {success}')
" 2>/dev/null || echo "0.0 0 0 0"
}

# extract_codex_metrics <repo-dir>
# Walks .codex-dispatch/runs/*/stdout.log for the most recent run; pulls the
# last thread/tokenUsage/updated and sums turn/completed durations.
# prints "in_tokens cached_in_tokens out_tokens reasoning_tokens codex_dur_ms"
extract_codex_metrics() {
  local repo="$1"
  local last_run
  last_run="$(find "$repo/.codex-dispatch/runs" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | sort | tail -n1)"
  if [ -z "$last_run" ] || [ ! -f "$last_run/stdout.log" ]; then
    echo "0 0 0 0 0"; return
  fi
  python3 -c "
import json, sys
in_tok = cached = out = reasoning = dur = 0
with open('$last_run/stdout.log') as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            m = json.loads(line)
        except Exception:
            continue
        meth = m.get('method')
        params = m.get('params', {})
        if meth == 'thread/tokenUsage/updated':
            total = params.get('tokenUsage', {}).get('total', {})
            in_tok = total.get('inputTokens', 0)
            cached = total.get('cachedInputTokens', 0)
            out = total.get('outputTokens', 0)
            reasoning = total.get('reasoningOutputTokens', 0)
        elif meth == 'turn/completed':
            dur += params.get('turn', {}).get('durationMs', 0) or 0
print(f'{in_tok} {cached} {out} {reasoning} {dur}')
"
}

run_direct() {
  local name="$1" repo="$2" stream="$3"
  local prompt acc test_cmd full
  prompt="${PROMPTS[$name]}"
  acc="${ACCEPTANCE[$name]}"
  test_cmd="python3 $REPO_ROOT/tests/sdk/quality/${name}.py"
  # Same info as the plugin leg: prompt + acceptance + test command. Direct
  # Claude can choose to run the test, iterate, or not — that judgment is
  # exactly the variable we want to measure against the plugin's structured
  # iterate-and-review loop.
  full="$prompt

ACCEPTANCE CRITERIA:
$acc

You may verify your implementation by running this command in the repo root:
  $test_cmd
The test exits 0 only on a perfect score, exits 1 with a list of failed cases otherwise.

Use file-editing tools directly. Then stop."
  # shellcheck disable=SC2086
  claude $bare_flag --print --output-format stream-json --verbose \
    --model "$MODEL" \
    --add-dir "$repo" --dangerously-skip-permissions \
    --max-budget-usd "$BUDGET" \
    </dev/null \
    "$full" \
    >"$stream" 2>"$stream.err" || true
}

run_plugin() {
  local name="$1" repo="$2" stream="$3"
  local prompt acc cmd test_cmd
  prompt="${PROMPTS[$name]}"
  acc="${ACCEPTANCE[$name]}"
  # The test command exits non-zero on any failed case so the reviewer
  # subagent sees "needs-changes" and codex iterates. We pass an absolute
  # path so the test runs correctly even though cwd is the throwaway repo.
  test_cmd="python3 $REPO_ROOT/tests/sdk/quality/${name}.py"
  cmd="/codex-dispatch:codex --max-iter $MAX_ITER --test-cmd \"$test_cmd\" --acceptance \"${acc//\"/\\\"}\" $prompt"
  # shellcheck disable=SC2086
  CODEX_DISPATCH_BIN="$CODEX_DISPATCH_BIN" CODEX_SANDBOX="$SANDBOX" \
    claude $bare_flag --print --output-format stream-json --verbose \
    --model "$MODEL" \
    --add-dir "$repo" --dangerously-skip-permissions \
    --max-budget-usd "$BUDGET" \
    --plugin-dir "$REPO_ROOT" \
    </dev/null \
    "$cmd" \
    >"$stream" 2>"$stream.err" || true
}

# ---------------------------------------------------------------------------
# Driver.
# ---------------------------------------------------------------------------

run_task() {
  local name="$1"
  printf '\n=== %s ===\n' "$name"

  # Direct.
  local d_repo d_stream
  d_repo="$(mktemp -d "$RESULTS_DIR/${name}-direct-XXXXXX")"
  cd "$d_repo"; git init -q -b main; git config user.email t@t; git config user.name t
  echo init > README.md; git add . && git commit -q -m init
  d_stream="$RESULTS_DIR/${name}-direct.jsonl"
  if [ "$DRY_RUN" = 1 ]; then
    note "[dry-run] would run: claude --print on prompt"
  else
    run_direct "$name" "$d_repo" "$d_stream"
  fi
  local d_metrics d_verdict
  d_metrics="$(extract_claude_metrics "$d_stream")"
  d_verdict="$(cd "$d_repo" && "verify_$name")"
  note "direct  : cost=\$$(echo "$d_metrics" | awk '{print $1}')  duration=$(echo "$d_metrics" | awk '{print $2}')ms  turns=$(echo "$d_metrics" | awk '{print $3}')  verdict=$d_verdict"

  # Plugin.
  local p_repo p_stream
  p_repo="$(mktemp -d "$RESULTS_DIR/${name}-plugin-XXXXXX")"
  cd "$p_repo"; git init -q -b main; git config user.email t@t; git config user.name t
  echo init > README.md; git add . && git commit -q -m init
  p_stream="$RESULTS_DIR/${name}-plugin.jsonl"
  if [ "$DRY_RUN" = 1 ]; then
    note "[dry-run] would run: claude --print --plugin-dir on slash command"
  else
    run_plugin "$name" "$p_repo" "$p_stream"
  fi
  local p_metrics p_codex p_verdict
  p_metrics="$(extract_claude_metrics "$p_stream")"
  p_codex="$(extract_codex_metrics "$p_repo")"
  p_verdict="$(cd "$p_repo" && "verify_$name")"
  note "plugin  : cost=\$$(echo "$p_metrics" | awk '{print $1}') (claude) codex_in=$(echo "$p_codex" | awk '{print $1}') codex_out=$(echo "$p_codex" | awk '{print $3}') codex_dur=$(echo "$p_codex" | awk '{print $5}')ms  total_dur=$(echo "$p_metrics" | awk '{print $2}')ms  turns=$(echo "$p_metrics" | awk '{print $3}')  verdict=$p_verdict"

  # Persist row. Pass values via env vars to avoid shell-quoting issues.
  TASK="$name" DIRECT_METRICS="$d_metrics" PLUGIN_METRICS="$p_metrics" \
  PLUGIN_CODEX="$p_codex" DIRECT_VERDICT="$d_verdict" PLUGIN_VERDICT="$p_verdict" \
  RESULTS_JSON="$RESULTS_JSON" \
  python3 <<'PYEOF'
import json, os
row = {
    'task': os.environ['TASK'],
    'direct': dict(zip(
        ['cost_usd', 'duration_ms', 'turns', 'is_success'],
        os.environ['DIRECT_METRICS'].split())),
    'plugin': {
        **dict(zip(
            ['claude_cost_usd', 'claude_duration_ms', 'claude_turns', 'claude_is_success'],
            os.environ['PLUGIN_METRICS'].split())),
        **dict(zip(
            ['codex_in_tokens', 'codex_cached_tokens', 'codex_out_tokens',
             'codex_reasoning_tokens', 'codex_duration_ms'],
            os.environ['PLUGIN_CODEX'].split())),
    },
    'direct_verdict': os.environ['DIRECT_VERDICT'],
    'plugin_verdict': os.environ['PLUGIN_VERDICT'],
}
path = os.environ['RESULTS_JSON']
existing = []
if os.path.exists(path):
    try:
        with open(path) as f:
            existing = json.load(f)
    except Exception:
        pass
existing = [r for r in existing if r.get('task') != row['task']]
existing.append(row)
with open(path, 'w') as f:
    json.dump(existing, f, indent=2)
PYEOF
}

# Skip the driver if we're being sourced (so test code can call the
# verify_* functions directly without paying for an API run).
(return 0 2>/dev/null) && return 0

# Select tasks.
case "$MODE" in
  trivial-only) selected=(trivial) ;;
  easy)         selected=("${TASK_ORDER[@]}") ;;
  hard)         selected=("${HARD_TASK_ORDER[@]}") ;;
  *) err "unknown COMPARE_MODE=$MODE"; exit 64 ;;
esac

printf '\ncompare-headless: model=%s budget=$%s/leg max-iter=%d mode=%s\n' "$MODEL" "$BUDGET" "$MAX_ITER" "$MODE"
printf '                  CODEX_DISPATCH_BIN=%s\n' "$CODEX_DISPATCH_BIN"
printf '                  CODEX_SANDBOX=%s\n' "$SANDBOX"

# Truncate results.json at start so each run is fresh.
echo '[]' > "$RESULTS_JSON"

for t in "${selected[@]}"; do
  run_task "$t"
done

printf '\n=== summary ===\n'
RESULTS_JSON="$RESULTS_JSON" python3 <<'PYEOF'
import json, os, re
with open(os.environ['RESULTS_JSON']) as f:
    rows = json.load(f)

def score(verdict: str) -> str:
    # "PASS 12/12" / "PARTIAL 8/12: ..." / "FAIL 0/?: ..." -> "12/12"
    m = re.search(r'(\d+)/(\d+|\?)', verdict)
    if m:
        tag = verdict.split()[0]
        return f"{tag[0]}{m.group(0)}"  # e.g. P12/12, F0/12, A8/12 (PARTIAL→A for "almost")
    return verdict.split()[0][0]  # legacy: just first char (P/F)

hdr = ('task', 'direct $', 'direct ms', 'direct score', 'plugin $', 'plugin ms', 'codex tok in/out', 'plugin score')
fmt = '| {:<16} | {:>9} | {:>9} | {:>13} | {:>9} | {:>9} | {:>17} | {:>13} |'
print(fmt.format(*hdr))
print('|' + '-'*18 + '|' + '-'*11 + '|' + '-'*11 + '|' + '-'*15 + '|' + '-'*11 + '|' + '-'*11 + '|' + '-'*19 + '|' + '-'*15 + '|')
for r in rows:
    d = r['direct']
    p = r['plugin']
    print(fmt.format(
        r['task'][:16],
        '$' + d.get('cost_usd', '?'),
        d.get('duration_ms', '?'),
        score(r['direct_verdict']),
        '$' + p.get('claude_cost_usd', '?'),
        p.get('claude_duration_ms', '?'),
        f"{p.get('codex_in_tokens', '?')}/{p.get('codex_out_tokens', '?')}",
        score(r['plugin_verdict'])))
PYEOF

printf '\nresults written to %s\n' "$RESULTS_JSON"
