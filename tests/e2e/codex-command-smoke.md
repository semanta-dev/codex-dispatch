# /codex end-to-end smoke

Manual procedure for sanity-checking the `/codex` slash command + reviewer
loop without touching a real Codex session. Bats covers the dispatch core
and iteration helper deterministically; this script catches integration
surfaces that only surface inside Claude Code (frontmatter parsing, `Task`
spawn shape, response parsing, final report).

## Prerequisites

- This repository checked out locally and installed as a Claude Code plugin
  (e.g., via `/plugin add /home/you/path/to/claude-code-codex-dispatch`).
- `bats`, `jq`, `git` on PATH.
- A scratch directory you don't mind nuking.

You do **not** need a real `codex` binary or a Codex API key — the smoke
script builds the JSON-RPC fake at `tests/fixtures/fake-appserver/` and
installs it as `codex` on PATH. The fake speaks `codex --version` (≥ 0.130.0)
and `codex app-server` so the v0.3.0 broker can drive it.

## Setup

```bash
PLUGIN_ROOT="/home/you/path/to/claude-code-codex-dispatch"

# Throwaway repo
SMOKE_REPO="$(mktemp -d)"
cd "$SMOKE_REPO"
git init -q -b main
git config user.email smoke@test
git config user.name "smoke"
echo "initial" > README.md
git add README.md && git commit -q -m "init"

# Build the fake codex (separate Go module) and install as `codex` on PATH
FAKE_BIN="$(mktemp -d)"
( cd "$PLUGIN_ROOT/tests/fixtures/fake-appserver" && go build -o "$FAKE_BIN/fake-appserver" . )
ln -s "$FAKE_BIN/fake-appserver" "$FAKE_BIN/codex"
export PATH="$FAKE_BIN:$PATH"

# Configure fake codex to make a tiny, harmless edit
export FAKE_CODEX_VERSION="0.130.0"
export FAKE_APPSERVER_EDIT="hello.txt:Hello, smoke"
export FAKE_APPSERVER_SESSION="smoke-thread-001"
```

## Run 1 — happy path

In Claude Code, with cwd set to `$SMOKE_REPO`:

```
/codex --max-iter 1 --no-tests --acceptance "hello.txt exists with greeting" add a hello.txt with a greeting
```

**Expected:**
- Final report has `verdict: pass`, `iterations: 1 / 1`, `files changed: hello.txt`, `session id: smoke-thread-001`, `fell back to fresh: false`.
- `$SMOKE_REPO/hello.txt` exists with the configured content.
- `$SMOKE_REPO/.codex-dispatch/runs/<ts>-<pid>/result.json` is well-formed JSON containing the spec fields (`error_message` is omitted on success).
- `$SMOKE_REPO/.codex-dispatch/runs/<ts>-<pid>/stdout.log` contains real-protocol notification lines: `{"method":"thread/started","params":...}`, `{"method":"turn/started",...}`, `{"method":"turn/completed",...}`.
- The report ends with the working-tree reminder.

## Run 2 — exhausted iterations

```bash
unset FAKE_APPSERVER_EDIT
export FAKE_APPSERVER_SESSION="smoke-thread-002"
```

```
/codex --max-iter 2 --no-tests --acceptance "must add a hello.txt" add a hello.txt with greeting
```

**Expected:**
- The reviewer flags `no-changes` on iteration 1; the loop terminates immediately (no second iteration is needed because `no-changes` is a hard fail).
- Final report has `verdict: fail`, `reason: no-changes`.

## Run 3 — stale resume fallback

```bash
export FAKE_APPSERVER_EDIT="from-fresh.txt:from fresh retry"
export FAKE_APPSERVER_STALE_RESUME="01010101-0101-0101-0101-010101010101"
```

Manually invoke the dispatch script with a stale session id (the slash
command itself doesn't accept a session id directly; use the script):

```bash
CODEX_TASK="t" CODEX_ACCEPTANCE="a" \
CODEX_SESSION_ID="$FAKE_APPSERVER_STALE_RESUME" \
"$PLUGIN_ROOT/scripts/dispatch-codex.sh"
```

**Expected:**
- `result.json.fell_back_to_fresh == true`.
- `from-fresh.txt` exists in the working tree.
- `stdout.log` contains the literal `==== fell back to fresh dispatch ====` marker.
  In the new format the stale-resume rejection itself is not echoed into the log
  (it's a typed JSON-RPC error response handled at the broker layer); the marker
  line and the subsequent fresh-attempt notifications are the visible evidence.

## Cleanup

```bash
# Kill any broker the smoke runs spawned for this throwaway repo.
if [ -f "$SMOKE_REPO/.codex-dispatch/broker.pid" ]; then
  kill "$(cat "$SMOKE_REPO/.codex-dispatch/broker.pid")" 2>/dev/null || true
fi
rm -rf "$SMOKE_REPO" "$FAKE_BIN"
unset FAKE_CODEX_VERSION FAKE_APPSERVER_EDIT FAKE_APPSERVER_SESSION FAKE_APPSERVER_STALE_RESUME
```

## What this does NOT cover

- Real codex CLI invocation (cost, network, auth).
- Real reviewer judgment (run `tests/reviewer/run-fixtures.sh` for that).
- The codex-dispatch subagent surface (Task 7) — covered by its own
  manual invocation procedure once that lands.
