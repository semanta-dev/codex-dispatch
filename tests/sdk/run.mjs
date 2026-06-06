#!/usr/bin/env node
// tests/sdk/run.mjs — Claude Agent SDK integration tests for the
// codex-dispatch plugin. Runs four real-Claude tests:
//
//   1. Plugin loads cleanly (slash command appears in init message).
//   2. codex-dispatch agent's underspecified short-circuit returns the
//      contract JSON when ACCEPTANCE CRITERIA is empty.
//   3. /codex-dispatch:codex actually invokes scripts/dispatch-codex.sh
//      via Bash (verified through a PreToolUse hook), with a fake codex
//      on PATH, and lands the edit in a throwaway working tree.
//   4. Reviewer fixture sweep — all 7 fixtures, agent's VERDICT/REASON
//      matches expected_verdict.txt.
//
// Skip behavior: if the SDK can't authenticate (no ANTHROPIC_API_KEY and
// no OAuth credentials reachable from a non-Claude-Code subprocess), the
// runner prints a clear skip message and exits 0 so CI without secrets
// passes.
//
// Usage:
//   cd tests/sdk
//   npm install
//   node run.mjs              # all tests
//   node run.mjs <name>...    # filter by test name substring(s)

import { query } from "@anthropic-ai/claude-agent-sdk";
import { mkdtempSync, rmSync, writeFileSync, readFileSync, existsSync, readdirSync, statSync, chmodSync } from "node:fs";
import { execSync } from "node:child_process";
import { tmpdir } from "node:os";
import path from "node:path";
import process from "node:process";

const REPO_ROOT = path.resolve(new URL("../../", import.meta.url).pathname);
const PLUGIN_PATH = REPO_ROOT;
const FIXTURES_DIR = path.join(REPO_ROOT, "tests/fixtures/reviewer");
const REVIEWER_AGENT = path.join(REPO_ROOT, "agents/codex-reviewer.md");
const MODEL = process.env.SDK_TEST_MODEL || "claude-sonnet-4-6";

let pass = 0, fail = 0, skipped = 0;
const failures = [];

const filter = process.argv.slice(2);
const wanted = (name) => filter.length === 0 || filter.some(f => name.includes(f));

function ok(name, msg = "") {
  console.log(`  ok  ${name}${msg ? " — " + msg : ""}`);
  pass++;
}
function bad(name, msg) {
  console.log(`  FAIL ${name} — ${msg}`);
  fail++;
  failures.push({ name, msg });
}

function makeRepo() {
  const dir = mkdtempSync(path.join(tmpdir(), "sdk-smoke-"));
  execSync("git init -q -b main", { cwd: dir });
  execSync("git config user.email s@s && git config user.name s", { cwd: dir, shell: "/bin/bash" });
  writeFileSync(path.join(dir, "README.md"), "initial\n");
  execSync("git add README.md && git commit -q -m init", { cwd: dir, shell: "/bin/bash" });
  return dir;
}

// Inline fake codex script (formerly tests/bats/helpers/fake-codex.sh, removed in v0.3.0).
// Mimics codex exec [OPTIONS] [PROMPT] / codex exec resume [OPTIONS] [SESSION_ID] [PROMPT].
const FAKE_CODEX_SCRIPT = `#!/usr/bin/env bash
set -euo pipefail
session="\${FAKE_CODEX_SESSION:-fake-thread-001}"
exit_code="\${FAKE_CODEX_EXIT:-0}"
if [ -n "\${FAKE_CODEX_ARGV_LOG:-}" ]; then
  printf '%s\\n' "$*" >> "\$FAKE_CODEX_ARGV_LOG"
fi
is_resume=0
for arg in "$@"; do
  if [ "$arg" = "resume" ]; then is_resume=1; break; fi
done
if [ "$is_resume" = 1 ] && [ -n "\${FAKE_CODEX_STALE_RESUME:-}" ]; then
  printf 'Error: thread/resume: thread/resume failed: no rollout found for thread id %s\\n' \\
    "\$FAKE_CODEX_STALE_RESUME" >&2
  exit 0
fi
if [ -n "\${FAKE_CODEX_EDIT:-}" ]; then
  path="\${FAKE_CODEX_EDIT%%:*}"
  content="\${FAKE_CODEX_EDIT#*:}"
  mkdir -p "$(dirname "$path")"
  printf '%s\\n' "$content" > "$path"
fi
if [ -n "\${FAKE_CODEX_SCRIPT:-}" ] && [ -f "\$FAKE_CODEX_SCRIPT" ]; then
  bash "\$FAKE_CODEX_SCRIPT" "$@"
fi
printf '{"type":"thread.started","thread_id":"%s"}\\n' "$session"
printf '{"type":"turn.completed"}\\n'
exit "$exit_code"
`;

function makeFakeCodexBin({ edit, session, exit, stale, argvLog }) {
  const binDir = mkdtempSync(path.join(tmpdir(), "fake-codex-bin-"));
  const codexPath = path.join(binDir, "codex");
  writeFileSync(codexPath, FAKE_CODEX_SCRIPT);
  chmodSync(codexPath, 0o755);
  const env = {};
  if (edit) env.FAKE_CODEX_EDIT = edit;
  if (session) env.FAKE_CODEX_SESSION = session;
  if (exit !== undefined) env.FAKE_CODEX_EXIT = String(exit);
  if (stale) env.FAKE_CODEX_STALE_RESUME = stale;
  if (argvLog) env.FAKE_CODEX_ARGV_LOG = argvLog;
  return { binDir, env };
}

function readReviewerAgentBody() {
  // Strip the YAML frontmatter — the SDK accepts a plain agent prompt.
  const raw = readFileSync(REVIEWER_AGENT, "utf8");
  const lines = raw.split("\n");
  let dashes = 0, start = 0;
  for (let i = 0; i < lines.length; i++) {
    if (lines[i] === "---") {
      dashes++;
      if (dashes === 2) { start = i + 1; break; }
    }
  }
  return lines.slice(start).join("\n").trim();
}

async function collect(it) {
  const out = { messages: [], result: null, init: null, errored: null };
  try {
    for await (const msg of it) {
      out.messages.push(msg);
      if (msg.type === "system" && msg.subtype === "init") out.init = msg;
      if (msg.type === "result") out.result = msg;
    }
  } catch (e) {
    out.errored = e;
  }
  return out;
}

function lastJsonBlock(text) {
  // Find the last balanced {...} in the response.
  let depth = 0, start = -1, end = -1;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (c === "{") {
      if (depth === 0) start = i;
      depth++;
    } else if (c === "}") {
      depth--;
      if (depth === 0) end = i;
    }
  }
  if (start === -1 || end === -1) return null;
  try {
    return JSON.parse(text.slice(start, end + 1));
  } catch {
    return null;
  }
}

function assistantText(messages) {
  const parts = [];
  for (const m of messages) {
    if (m.type === "assistant" && m.message?.content) {
      for (const c of m.message.content) {
        if (c.type === "text") parts.push(c.text);
      }
    }
  }
  return parts.join("\n");
}

// ─── tests ────────────────────────────────────────────────────────────────

async function testPluginLoads() {
  const name = "plugin loads + /codex-dispatch:codex registered";
  if (!wanted(name)) return;
  const out = await collect(query({
    prompt: "say ok and exit",
    options: {
      plugins: [{ type: "local", path: PLUGIN_PATH }],
      maxTurns: 1,
      model: MODEL,
    },
  }));
  if (out.errored) return bad(name, `query errored: ${out.errored.message}`);
  if (!out.init) return bad(name, "no init message");

  const plugins = out.init.plugins || [];
  const cmds = out.init.slash_commands || [];
  const hasPlugin = plugins.some(p => p.name === "codex-dispatch");
  const hasCmd = cmds.some(c => c.includes("codex-dispatch") && c.includes("codex"));
  if (!hasPlugin) return bad(name, `plugin not in init.plugins; got ${JSON.stringify(plugins)}`);
  if (!hasCmd) return bad(name, `/codex-dispatch:codex not in slash_commands`);
  ok(name, `${plugins.length} plugin(s), command registered`);
}

async function testUnderspecified() {
  const name = "codex-dispatch underspecified short-circuit returns contract JSON";
  if (!wanted(name)) return;
  // Inject the agent body as system prompt and feed the underspecified
  // request as the user message. Tests the agent's input validation
  // contract directly, not the orchestration that delegates to it.
  const agentBody = readFileSync(path.join(REPO_ROOT, "agents/codex-dispatch.md"), "utf8");
  const lines = agentBody.split("\n");
  let dashes = 0, start = 0;
  for (let i = 0; i < lines.length; i++) {
    if (lines[i] === "---") { dashes++; if (dashes === 2) { start = i + 1; break; } }
  }
  const body = lines.slice(start).join("\n").trim();

  const out = await collect(query({
    prompt: `TASK\nadd a /healthz endpoint to the FastAPI app\n\nACCEPTANCE CRITERIA\n\n`,
    options: {
      model: MODEL,
      maxTurns: 4,
      systemPrompt: body,
      allowedTools: ["Read", "Bash"],
      settingSources: [],
    },
  }));
  if (out.errored) return bad(name, `query errored: ${out.errored.message}\nResponse tail: ${assistantText(out.messages).slice(-400)}`);
  const text = assistantText(out.messages);
  const json = lastJsonBlock(text);
  if (!json) return bad(name, `no JSON block in response. Full response:\n${text}`);
  if (json.status !== "fail") return bad(name, `expected status=fail, got ${json.status}`);
  if (json.iterations !== 0) return bad(name, `expected iterations=0, got ${json.iterations}`);
  if (!Array.isArray(json.files_changed) || json.files_changed.length !== 0) return bad(name, `expected empty files_changed, got ${JSON.stringify(json.files_changed)}`);
  if (!/underspecified/i.test(json.summary || "")) return bad(name, `expected summary to mention underspecified, got: ${json.summary}`);
  ok(name, `summary: "${json.summary.slice(0, 60)}"`);
}

async function testSlashCommandE2E() {
  const name = "/codex-dispatch:codex actually invokes dispatch-codex.sh";
  if (!wanted(name)) return;
  const repo = makeRepo();
  const argvLog = path.join(repo, "..", `argv-${process.pid}.log`);
  const { binDir, env } = makeFakeCodexBin({
    edit: "hello.txt:Hello from SDK e2e",
    session: "sdk-e2e-thread-001",
    argvLog,
  });

  // Save and override env for the SDK call so the spawned claude inherits PATH and FAKE_*.
  const orig = {};
  for (const [k, v] of Object.entries({ ...env, PATH: `${binDir}:${process.env.PATH}` })) {
    orig[k] = process.env[k];
    process.env[k] = v;
  }

  // Capture all Bash invocations Claude makes via PreToolUse hook so we can
  // verify dispatch-codex.sh actually ran.
  const bashCalls = [];
  const out = await collect(query({
    prompt: `/codex-dispatch:codex --max-iter 1 --no-tests --acceptance "hello.txt exists with a greeting" --files README.md add a hello.txt with a greeting`,
    options: {
      cwd: repo,
      model: MODEL,
      maxTurns: 25,
      plugins: [{ type: "local", path: PLUGIN_PATH }],
      permissionMode: "bypassPermissions",
      settingSources: [],
      hooks: {
        PreToolUse: [{
          matcher: "Bash",
          hooks: [async (input) => {
            const cmd = input?.tool_input?.command || "";
            bashCalls.push(cmd);
            return {};
          }],
        }],
      },
    },
  }));

  // Restore env.
  for (const [k, v] of Object.entries(orig)) {
    if (v === undefined) delete process.env[k]; else process.env[k] = v;
  }

  let errMsg = null;
  try {
    if (out.errored) throw new Error(`query errored: ${out.errored.message}`);
    const dispatched = bashCalls.some(c => c.includes("dispatch-codex.sh"));
    const fileExists = existsSync(path.join(repo, "hello.txt"));
    const fileContent = fileExists ? readFileSync(path.join(repo, "hello.txt"), "utf8") : "";
    const runsDir = path.join(repo, ".codex-dispatch", "runs");
    const hasRunDir = existsSync(runsDir) && readdirSync(runsDir).length > 0;

    if (!dispatched) {
      throw new Error(`Bash never invoked dispatch-codex.sh. Bash calls seen:\n${bashCalls.map((c, i) => `  ${i + 1}. ${c.slice(0, 120)}`).join("\n") || "  (no Bash calls at all)"}`);
    }
    if (!hasRunDir) throw new Error(`no .codex-dispatch/runs/ directory in ${repo}`);
    if (!fileExists) throw new Error(`hello.txt not created`);
    if (!fileContent.includes("Hello")) throw new Error(`hello.txt content unexpected: "${fileContent.slice(0, 60)}"`);

    // result.json sanity
    const runs = readdirSync(runsDir);
    const resultPath = path.join(runsDir, runs[0], "result.json");
    if (!existsSync(resultPath)) throw new Error(`result.json missing at ${resultPath}`);
    const result = JSON.parse(readFileSync(resultPath, "utf8"));
    if (result.session_id !== "sdk-e2e-thread-001") throw new Error(`session_id mismatch: ${result.session_id}`);
    if (!Array.isArray(result.files_changed) || !result.files_changed.includes("hello.txt")) throw new Error(`files_changed missing hello.txt: ${JSON.stringify(result.files_changed)}`);
  } catch (e) {
    errMsg = e.message;
  } finally {
    rmSync(repo, { recursive: true, force: true });
    rmSync(binDir, { recursive: true, force: true });
  }

  if (errMsg) return bad(name, errMsg);
  ok(name, `dispatch-codex.sh invoked; result.json + working tree OK`);
}

async function testReviewerFixtures() {
  const name = "reviewer fixture sweep (7 fixtures)";
  if (!wanted(name)) return;
  const fixtures = readdirSync(FIXTURES_DIR).filter(f => statSync(path.join(FIXTURES_DIR, f)).isDirectory()).sort();
  if (fixtures.length === 0) return bad(name, "no fixtures found");

  const agentBody = readReviewerAgentBody();
  let fixturePass = 0, fixtureFail = 0;
  const fixtureFails = [];

  for (const fix of fixtures) {
    const dir = path.join(FIXTURES_DIR, fix);
    const task = readFileSync(path.join(dir, "task.txt"), "utf8");
    const acceptance = readFileSync(path.join(dir, "acceptance.txt"), "utf8");
    const constraints = existsSync(path.join(dir, "constraints.txt")) ? readFileSync(path.join(dir, "constraints.txt"), "utf8") : "";
    const diff = readFileSync(path.join(dir, "diff.patch"), "utf8");
    const policy = existsSync(path.join(dir, "test-policy.txt")) ? readFileSync(path.join(dir, "test-policy.txt"), "utf8").trim() : "skip";
    const testCmd = existsSync(path.join(dir, "test-cmd.txt")) ? readFileSync(path.join(dir, "test-cmd.txt"), "utf8").trim() : "";
    const expected = readFileSync(path.join(dir, "expected_verdict.txt"), "utf8");
    const expectedVerdict = (expected.match(/^VERDICT:\s*(.*)$/m) || [, ""])[1].trim();
    const expectedReason = (expected.match(/^REASON:\s*(.*)$/m) || [, ""])[1].trim();

    const prompt = [
      `TASK\n${task.trim()}`,
      `ACCEPTANCE CRITERIA\n${acceptance.trim()}`,
      constraints ? `CONSTRAINTS\n${constraints.trim()}` : null,
      `DIFF PATH\n${path.join(dir, "diff.patch")}`,
      "DIFF CONTENT",
      "```",
      diff.trimEnd(),
      "```",
      `TEST POLICY\n${policy}`,
      testCmd ? `TEST CMD\n${testCmd}` : null,
    ].filter(Boolean).join("\n\n");

    // Run each fixture in a clean tempdir so the reviewer can't be misled by
    // unrelated files in the SDK runner's cwd. The DIFF CONTENT block in the
    // prompt is the source of truth; the reviewer should never need to look
    // at on-disk files in this context.
    const cleanCwd = mkdtempSync(path.join(tmpdir(), "fixture-cwd-"));
    let out;
    let cwdMutated = false;
    let cwdMutationDetail = "";
    try {
      out = await collect(query({
        prompt,
        options: {
          cwd: cleanCwd,
          model: MODEL,
          maxTurns: 12,
          systemPrompt: agentBody,
          // Reviewer body's read-only mandate forbids Bash file mutations.
          // We grant Bash here only so test-command fixtures can exercise it
          // (TEST POLICY: run + TEST CMD: false). Read/Grep/Glob round out
          // the read-only toolset.
          allowedTools: ["Read", "Bash", "Grep", "Glob"],
          // Use systemPrompt (replaces) instead of appendSystemPrompt — full
          // override gives the agent body its own rules without competing
          // against Claude Code's default user-facing tone.
        },
      }));
      // Post-condition: the reviewer's read-only mandate forbids it from
      // mutating the working tree. Catches regressions where a future agent
      // body weakens the rule and the model starts writing files again.
      const remaining = readdirSync(cleanCwd);
      if (remaining.length > 0) {
        cwdMutated = true;
        cwdMutationDetail = `reviewer wrote into cwd: ${remaining.join(", ")}`;
      }
    } finally {
      rmSync(cleanCwd, { recursive: true, force: true });
    }

    if (out.errored) { fixtureFails.push(`${fix}: query errored: ${out.errored.message}`); fixtureFail++; continue; }
    if (cwdMutated) { fixtureFails.push(`${fix}: ${cwdMutationDetail}`); fixtureFail++; continue; }
    const text = assistantText(out.messages);
    // Strip markdown decoration chars (bold/italic/blockquote/code) so the
    // strict labels match even when the model wraps them in markdown.
    const cleaned = text.replace(/[*>`#]/g, "");
    const verdictMatches = [...cleaned.matchAll(/^[ \t]*VERDICT[ \t]*:[ \t]*([A-Za-z\- ]+)[ \t]*$/gm)];
    const reasonMatches = [...cleaned.matchAll(/^[ \t]*REASON[ \t]*:[ \t]*([A-Za-z\-]*)[ \t]*$/gm)];
    const gotVerdict = verdictMatches.length ? verdictMatches.at(-1)[1].trim() : "";
    const gotReason = reasonMatches.length ? reasonMatches.at(-1)[1].trim() : "";
    if (gotVerdict === expectedVerdict && gotReason === expectedReason) {
      fixturePass++;
    } else {
      const debugTail = text.slice(-400).replace(/\n/g, "\n      ");
      fixtureFails.push(`${fix}: expected ${expectedVerdict}/${expectedReason}, got "${gotVerdict}"/"${gotReason}"\n      response tail: ${debugTail || "(empty)"}`);
      fixtureFail++;
    }
  }

  if (fixtureFail > 0) return bad(name, `${fixturePass}/${fixtures.length} matched. Mismatches:\n    ${fixtureFails.join("\n    ")}`);
  ok(name, `${fixturePass}/${fixtures.length} matched`);
}

// ─── runner ───────────────────────────────────────────────────────────────

async function main() {
  console.log("Claude Agent SDK integration tests for codex-dispatch plugin");
  console.log(`Model: ${MODEL}`);
  console.log("");

  // Quick auth sanity: try a one-line query and skip everything if we can't auth.
  if (!process.env.SDK_TEST_FORCE) {
    try {
      const probe = await collect(query({ prompt: "ok", options: { maxTurns: 1, model: MODEL } }));
      if (probe.errored) {
        console.log(`SDK auth probe failed: ${probe.errored.message}`);
        console.log("Skipping all SDK tests. Set ANTHROPIC_API_KEY or run inside a Claude Code session with OAuth.");
        process.exit(0);
      }
    } catch (e) {
      console.log(`SDK auth probe threw: ${e.message}`);
      process.exit(0);
    }
  }

  console.log("Running tests:");
  await testPluginLoads();
  await testUnderspecified();
  await testSlashCommandE2E();
  await testReviewerFixtures();

  console.log("");
  console.log(`Result: ${pass} pass, ${fail} fail`);
  void skipped;
  if (failures.length) {
    console.log("");
    console.log("Failures:");
    for (const f of failures) console.log(`  - ${f.name}: ${f.msg}`);
  }
  process.exit(fail > 0 ? 1 : 0);
}

main().catch((e) => { console.error("runner crashed:", e); process.exit(2); });
