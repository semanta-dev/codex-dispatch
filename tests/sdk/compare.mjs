#!/usr/bin/env node
// tests/sdk/compare.mjs — head-to-head: direct Claude vs codex-dispatch
// plugin on the same task, in throwaway repos.
//
// For each task we run two scenarios:
//   direct  — Claude with full filesystem tools (Read/Write/Edit/Bash/...).
//             One Anthropic call cycle; Claude does the work itself.
//   plugin  — Claude invokes `/codex-dispatch:codex --max-iter 1` which
//             spawns the dispatch script (real codex CLI, not the fake)
//             and the codex-reviewer subagent.
//
// We capture from each:
//   - Anthropic usage (input/output/cache tokens, total cost USD) from the
//     `result` message at the end of the SDK stream.
//   - Codex usage (input/output/reasoning tokens) by parsing the codex
//     `turn.completed` JSONL events out of every run's stdout.log.
//   - Wall-clock time.
//   - Quality verdict: a task-specific verifier inspects the working tree.
//
// One run per task per scenario — model judgment is variable so single-run
// results are anecdotal. Real cost: a few cents in Anthropic + a few cents
// in OpenAI per pass.

import { query } from "@anthropic-ai/claude-agent-sdk";
import { mkdtempSync, rmSync, writeFileSync, readFileSync, existsSync, readdirSync } from "node:fs";
import { execSync } from "node:child_process";
import { tmpdir } from "node:os";
import path from "node:path";
import process from "node:process";

const REPO_ROOT = path.resolve(new URL("../../", import.meta.url).pathname);
const MODEL = process.env.SDK_TEST_MODEL || "claude-sonnet-4-6";

const PLUGIN_PATH = REPO_ROOT;

const COMPARE_MODE = process.env.COMPARE_MODE || (process.env.COMPARE_HARD === "1" ? "hard" : "easy");
const MAX_ITER = parseInt(process.env.COMPARE_MAX_ITER || (COMPARE_MODE === "easy" ? "1" : "3"), 10);
const JUDGE_RUNS = parseInt(process.env.JUDGE_RUNS || "0", 10);
const JUDGE_MODEL = process.env.JUDGE_MODEL || MODEL;
// JUDGE_PROVIDER selects the judge backend:
//   "claude" (default) — Claude Agent SDK
//   "synthetic"        — synthetic.dev OpenAI-compatible API; useful for a
//                        third-party judge that isn't biased toward Claude
//                        output style. Requires SYNTHETIC_API_KEY.
const JUDGE_PROVIDER = process.env.JUDGE_PROVIDER || "claude";
const SYNTHETIC_BASE = process.env.SYNTHETIC_BASE_URL || "https://api.synthetic.new/openai/v1";
const SYNTHETIC_KEY = process.env.SYNTHETIC_API_KEY;
// Per-1M-token prices for known synthetic models, used to estimate cost.
// Update if synthetic's pricing changes.
const SYNTHETIC_PRICES = {
  "hf:deepseek-ai/DeepSeek-V3.2": { input: 0.27, output: 0.40 },
  "hf:deepseek-ai/DeepSeek-V3.1": { input: 0.56, output: 1.68 },
  "hf:deepseek-ai/DeepSeek-V3.1-Terminus": { input: 0.56, output: 1.68 },
};

const EASY_TASKS = [
  {
    name: "trivial",
    description: "create hello.txt",
    prompt: "Create a file hello.txt with the exact content `Hello, World!` (single line, no extra whitespace).",
    acceptance: "hello.txt exists with content Hello, World!",
    verify: (cwd) => {
      const p = path.join(cwd, "hello.txt");
      if (!existsSync(p)) return { ok: false, reason: "hello.txt missing" };
      const content = readFileSync(p, "utf8").trim();
      if (content !== "Hello, World!") return { ok: false, reason: `content mismatch: ${JSON.stringify(content)}` };
      return { ok: true };
    },
  },
  {
    name: "small",
    description: "reverse_string function",
    prompt: "Add a Python module src/strings.py with a function reverse_string(s) that returns the reversed string. Pure function, no side effects.",
    acceptance: "src/strings.py defines reverse_string; reverse_string('hello') returns 'olleh'; reverse_string('') returns ''",
    verify: (cwd) => {
      const p = path.join(cwd, "src/strings.py");
      if (!existsSync(p)) return { ok: false, reason: "src/strings.py missing" };
      try {
        const tester = `import sys; sys.path.insert(0, 'src'); from strings import reverse_string; assert reverse_string('hello') == 'olleh', repr(reverse_string('hello')); assert reverse_string('') == '', repr(reverse_string('')); print('OK')`;
        const result = execSync(`python3 -c ${JSON.stringify(tester)}`, { cwd, encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] });
        return result.trim() === "OK" ? { ok: true } : { ok: false, reason: result.trim() };
      } catch (e) {
        return { ok: false, reason: `exec error: ${(e.stderr || e.message || "").slice(0, 200)}` };
      }
    },
  },
  {
    name: "medium",
    description: "Stack class with 5 methods",
    prompt: "Implement a Stack class in src/stack.py with these methods: push(value), pop() (returns top, raises if empty), peek() (returns top without removing), size() (returns count), is_empty() (returns bool). Standard LIFO semantics.",
    acceptance: "src/stack.py defines Stack with push/pop/peek/size/is_empty; LIFO order; pop and peek raise IndexError on empty stack",
    verify: (cwd) => {
      const p = path.join(cwd, "src/stack.py");
      if (!existsSync(p)) return { ok: false, reason: "src/stack.py missing" };
      const tester = [
        "import sys",
        "sys.path.insert(0, 'src')",
        "from stack import Stack",
        "s = Stack()",
        "assert s.is_empty() and s.size() == 0",
        "try: s.pop()",
        "except Exception: pass",
        "else: raise AssertionError('expected exception on empty pop')",
        "s.push(1); s.push(2); s.push(3)",
        "assert s.size() == 3, s.size()",
        "assert s.peek() == 3, s.peek()",
        "assert s.pop() == 3",
        "assert s.pop() == 2",
        "assert s.size() == 1",
        "assert not s.is_empty()",
        "print('OK')",
      ];
      const testerPath = path.join(cwd, "_verify_stack.py");
      writeFileSync(testerPath, tester.join("\n"));
      try {
        const result = execSync(`python3 _verify_stack.py`, { cwd, encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] });
        return result.trim() === "OK" ? { ok: true } : { ok: false, reason: result.trim() };
      } catch (e) {
        return { ok: false, reason: `exec error: ${(e.stderr || e.message || "").toString().slice(0, 200)}` };
      } finally {
        rmSync(testerPath, { force: true });
      }
    },
  },
];

const HARD_TASKS = [
  {
    name: "csv-parser",
    description: "RFC-4180-ish CSV parser with quoted-field edge cases",
    prompt: "Implement parse_csv(text) in src/csvparser.py. Returns a list of rows; each row is a list of field strings. Must correctly handle: simple comma-separated values; double-quoted fields; commas inside quoted fields; literal newlines inside quoted fields; escaped double-quote inside a quoted field (the RFC-4180 \"\"  pattern, two double-quotes representing one); trailing newline at end of input (should not produce an extra empty row); empty fields. Pure function, stdlib only.",
    acceptance: [
      'parse_csv("a,b,c") == [["a", "b", "c"]]',
      'parse_csv("\\"a,b\\",c") == [["a,b", "c"]]',
      'parse_csv("\\"a\\"\\"b\\",c") == [["a\\"b", "c"]]',
      'parse_csv("a,\\"line1\\nline2\\",b") == [["a", "line1\\nline2", "b"]]',
      'parse_csv("a\\nb") == [["a"], ["b"]]',
      'parse_csv("a,,b") == [["a", "", "b"]]',
      'parse_csv("a\\n") == [["a"]]  # trailing newline does not add an empty row',
      'parse_csv("") == []',
    ].join("; "),
    verify: (cwd) => {
      const p = path.join(cwd, "src/csvparser.py");
      if (!existsSync(p)) return { ok: false, reason: "src/csvparser.py missing" };
      const tester = [
        "import sys",
        "sys.path.insert(0, 'src')",
        "from csvparser import parse_csv",
        "cases = [",
        "    ('a,b,c',                  [['a','b','c']]),",
        "    ('\"a,b\",c',                [['a,b','c']]),",
        "    ('\"a\"\"b\",c',             [['a\"b','c']]),",
        "    ('a,\"line1\\nline2\",b',    [['a','line1\\nline2','b']]),",
        "    ('a\\nb',                  [['a'],['b']]),",
        "    ('a,,b',                   [['a','','b']]),",
        "    ('a\\n',                   [['a']]),",
        "    ('',                       []),",
        "]",
        "for inp, want in cases:",
        "    got = parse_csv(inp)",
        "    assert got == want, f'parse_csv({inp!r}) = {got!r}, want {want!r}'",
        "print('OK')",
      ];
      const testerPath = path.join(cwd, "_verify_csv.py");
      writeFileSync(testerPath, tester.join("\n"));
      try {
        const result = execSync(`python3 _verify_csv.py`, { cwd, encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] });
        return result.trim() === "OK" ? { ok: true } : { ok: false, reason: result.trim() };
      } catch (e) {
        return { ok: false, reason: `exec error: ${(e.stderr || e.message || "").toString().slice(0, 300)}` };
      } finally {
        rmSync(testerPath, { force: true });
      }
    },
  },
  {
    name: "lru-cache",
    description: "LRUCache with O(1) get/put + correct LRU semantics",
    prompt: "Implement LRUCache in src/lru.py with __init__(capacity), get(key), put(key, value). get returns the value or -1 if absent and counts as a recency touch (the key becomes most-recently-used). put inserts or updates; if at capacity and inserting a new key, evict the least-recently-used key. Updating an existing key with put must not evict anything. Both get and put must be O(1) average — implement using a hash + doubly-linked list or collections.OrderedDict.",
    acceptance: [
      'capacity=2; put(1,1); put(2,2); get(1)==1; put(3,3) evicts 2; get(2)==-1; get(3)==3',
      'put(1,1); put(1,2) updates value; get(1)==2; size unchanged',
      'capacity=1 works correctly',
      'get on missing key returns -1 and does not change state',
    ].join("; "),
    verify: (cwd) => {
      const p = path.join(cwd, "src/lru.py");
      if (!existsSync(p)) return { ok: false, reason: "src/lru.py missing" };
      const tester = [
        "import sys",
        "sys.path.insert(0, 'src')",
        "from lru import LRUCache",
        "# spec test from leetcode 146",
        "c = LRUCache(2)",
        "c.put(1,1); c.put(2,2)",
        "assert c.get(1) == 1, c.get(1)",
        "c.put(3,3)  # evicts 2",
        "assert c.get(2) == -1, c.get(2)",
        "c.put(4,4)  # evicts 1 (was lru after the get)",
        "assert c.get(1) == -1, c.get(1)",
        "assert c.get(3) == 3, c.get(3)",
        "assert c.get(4) == 4, c.get(4)",
        "# update should not evict",
        "c2 = LRUCache(2)",
        "c2.put(1,'a'); c2.put(2,'b'); c2.put(1,'A')",
        "assert c2.get(1) == 'A', c2.get(1)",
        "assert c2.get(2) == 'b', c2.get(2)",
        "# capacity 1",
        "c3 = LRUCache(1)",
        "c3.put(1,1); c3.put(2,2)",
        "assert c3.get(1) == -1",
        "assert c3.get(2) == 2",
        "# missing key returns -1 without state change",
        "c4 = LRUCache(2)",
        "c4.put(1,1)",
        "assert c4.get(99) == -1",
        "c4.put(2,2); c4.put(3,3)",
        "assert c4.get(1) == -1, 'key 1 should still be evicted'",
        "print('OK')",
      ];
      const testerPath = path.join(cwd, "_verify_lru.py");
      writeFileSync(testerPath, tester.join("\n"));
      try {
        const result = execSync(`python3 _verify_lru.py`, { cwd, encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] });
        return result.trim() === "OK" ? { ok: true } : { ok: false, reason: result.trim() };
      } catch (e) {
        return { ok: false, reason: `exec error: ${(e.stderr || e.message || "").toString().slice(0, 300)}` };
      } finally {
        rmSync(testerPath, { force: true });
      }
    },
  },
  {
    name: "merge-intervals",
    description: "merge_intervals on date strings, with adjacency rule",
    prompt: "Implement merge_intervals(intervals) in src/intervals.py. intervals is a list of (start, end) tuples where each is an ISO date string YYYY-MM-DD. Endpoints are inclusive. Two intervals merge if they overlap OR are adjacent (i.e., one ends the day before the other starts). Result must be sorted by start date and contain non-overlapping, non-adjacent intervals. Handle empty input. Do not mutate the argument. Use stdlib datetime; no external deps.",
    acceptance: [
      'merge_intervals([]) == []',
      'merge_intervals([("2024-01-01","2024-01-05")]) == [("2024-01-01","2024-01-05")]',
      'overlap merges: [(1-1,1-5),(1-3,1-10)] -> [(1-1,1-10)]',
      'adjacency merges: [(1-1,1-4),(1-5,1-10)] -> [(1-1,1-10)] (1-4 and 1-5 are adjacent days)',
      'gap does NOT merge: [(1-1,1-3),(1-5,1-10)] -> [(1-1,1-3),(1-5,1-10)]',
      'unsorted input: result is sorted by start',
      'completely contained: [(1-1,1-31),(1-10,1-15)] -> [(1-1,1-31)]',
    ].join("; "),
    verify: (cwd) => {
      const p = path.join(cwd, "src/intervals.py");
      if (!existsSync(p)) return { ok: false, reason: "src/intervals.py missing" };
      const tester = [
        "import sys",
        "sys.path.insert(0, 'src')",
        "from intervals import merge_intervals",
        "def t(inp, want, name):",
        "    got = merge_intervals(inp)",
        "    got = [tuple(x) for x in got]  # accept tuple or list",
        "    want = [tuple(x) for x in want]",
        "    assert got == want, f'{name}: got {got}, want {want}'",
        "t([], [], 'empty')",
        "t([('2024-01-01','2024-01-05')], [('2024-01-01','2024-01-05')], 'single')",
        "t([('2024-01-01','2024-01-05'),('2024-01-03','2024-01-10')], [('2024-01-01','2024-01-10')], 'overlap')",
        "t([('2024-01-01','2024-01-04'),('2024-01-05','2024-01-10')], [('2024-01-01','2024-01-10')], 'adjacency')",
        "t([('2024-01-01','2024-01-03'),('2024-01-05','2024-01-10')], [('2024-01-01','2024-01-03'),('2024-01-05','2024-01-10')], 'gap')",
        "t([('2024-01-05','2024-01-10'),('2024-01-01','2024-01-03')], [('2024-01-01','2024-01-03'),('2024-01-05','2024-01-10')], 'unsorted')",
        "t([('2024-01-01','2024-01-31'),('2024-01-10','2024-01-15')], [('2024-01-01','2024-01-31')], 'contained')",
        "# argument not mutated",
        "arg = [('2024-01-01','2024-01-05'),('2024-01-03','2024-01-10')]",
        "before = list(arg)",
        "_ = merge_intervals(arg)",
        "assert arg == before, f'argument was mutated: {arg!r} != {before!r}'",
        "print('OK')",
      ];
      const testerPath = path.join(cwd, "_verify_intervals.py");
      writeFileSync(testerPath, tester.join("\n"));
      try {
        const result = execSync(`python3 _verify_intervals.py`, { cwd, encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] });
        return result.trim() === "OK" ? { ok: true } : { ok: false, reason: result.trim() };
      } catch (e) {
        return { ok: false, reason: `exec error: ${(e.stderr || e.message || "").toString().slice(0, 300)}` };
      } finally {
        rmSync(testerPath, { force: true });
      }
    },
  },
];

// ─── spec-writing tasks ────────────────────────────────────────────────────
// Design-doc generation is where direct Claude often stalls or
// scope-creeps into implementation. These tasks demand sustained
// focus on a single document and judgment on tradeoffs, not code.
//
// Verification is rubric-based: file exists, required section headers
// present, key concepts mentioned, minimum word count per section.
// Cosmetic style isn't graded — coverage and substance are.

function checkRubric(filePath, rubric) {
  if (!existsSync(filePath)) return { ok: false, reason: `${path.basename(filePath)} missing` };
  const text = readFileSync(filePath, "utf8");
  const wordCount = text.trim().split(/\s+/).length;
  if (wordCount < rubric.minWords) return { ok: false, reason: `too short: ${wordCount} words (need ${rubric.minWords})` };

  const missingSections = rubric.requiredSections.filter(rx => !rx.test(text));
  if (missingSections.length > 0) {
    return { ok: false, reason: `missing required sections (${missingSections.length}/${rubric.requiredSections.length}): ${missingSections.map(rx => rx.source).slice(0, 3).join(", ")}` };
  }

  const missingConcepts = rubric.requiredConcepts.filter(rx => !rx.test(text));
  if (missingConcepts.length > 0) {
    return { ok: false, reason: `missing key concepts (${missingConcepts.length}/${rubric.requiredConcepts.length}): ${missingConcepts.map(rx => rx.source).slice(0, 5).join(", ")}` };
  }

  return { ok: true, words: wordCount };
}

const SPEC_TASKS = [
  {
    name: "rate-limiter",
    description: "rate limiter service design doc",
    deliverableFile: "docs/rate-limiter.md",
    prompt: "Write `docs/rate-limiter.md`, a complete design specification for a distributed rate-limiter service. The doc must cover: goals & non-goals; core invariants; data model and storage choice; the algorithm (compare token bucket and sliding window — discuss precision, burstiness, memory cost; pick one with justification); the public API surface (key shapes, response shapes, error semantics); how to handle distributed coordination (clock skew, race conditions, monotonic vs wall clock); failure modes and degradation strategy; observability (metrics + key dashboards); operational concerns (warm-up, capacity sizing); open questions. Sustained design thinking, not code. Target 1500-3000 words.",
    acceptance: "docs/rate-limiter.md exists; covers goals, invariants, data model, algorithm comparison (token bucket vs sliding window with chosen winner and justification), API, distributed coordination, failure modes, observability, open questions; >= 1200 words",
    verify: (cwd) => checkRubric(path.join(cwd, "docs/rate-limiter.md"), {
      minWords: 1200,
      requiredSections: [
        /^#+\s.*goals?[\s:]/im,
        /^#+\s.*(invariants?|constraints?)/im,
        /^#+\s.*(data model|storage|state)/im,
        /^#+\s.*(algorithm|approach)/im,
        /^#+\s.*(api|interface|surface)/im,
        /^#+\s.*(failure|errors?|degrad)/im,
        /^#+\s.*(observab|metrics|monitoring)/im,
        /^#+\s.*(open questions|risks?|unknowns)/im,
      ],
      requiredConcepts: [
        /token bucket/i,
        /sliding window/i,
        /clock|time/i,
        /race condition|concurren|atomic|lock/i,
        /metric|counter|gauge|histogram/i,
        /redis|memcach|database|datastore|store/i,
      ],
    }),
  },
  {
    name: "pagination",
    description: "pagination strategy spec for a list API",
    deliverableFile: "docs/pagination.md",
    prompt: "Write `docs/pagination.md`, a thorough spec for the pagination strategy of a public REST list-API serving millions of users. Compare offset-based vs cursor-based vs keyset pagination explicitly with their tradeoffs (ordering guarantees, drift on insert/delete, deep-page performance, total-count semantics, caching friendliness). Pick one and justify. Then specify: the request/response shape, sort-stability requirements, how to handle filtering combined with pagination, edge cases (empty results, item deleted between pages, item inserted between pages), and migration strategy from a hypothetical existing offset-based system. 1200-2500 words.",
    acceptance: "docs/pagination.md exists; compares offset/cursor/keyset; picks one with justification; specifies request/response shapes; covers sort stability, filter+paginate interaction, mid-traversal mutations, migration from offset",
    verify: (cwd) => checkRubric(path.join(cwd, "docs/pagination.md"), {
      minWords: 1200,
      requiredSections: [
        /^#+\s.*(approach|strategy|comparison|options)/im,
        /^#+\s.*(api|request|response|shape|interface)/im,
        /^#+\s.*(edge cases?|race|drift|consistency|mutation)/im,
        /^#+\s.*(migration|rollout|cutover|transition)/im,
      ],
      requiredConcepts: [
        /offset/i,
        /cursor/i,
        /keyset|seek pagination/i,
        /sort stab|stable order|tie-break/i,
        /filter/i,
        /(insert|delete|mutation).*between|drift/i,
      ],
    }),
  },
  {
    name: "feature-flags",
    description: "feature-flag system design",
    deliverableFile: "docs/feature-flags.md",
    prompt: "Write `docs/feature-flags.md`, the design for an internal feature-flag service. Cover: motivation; flag lifecycle (creation, ramp, gradual rollout, cleanup); evaluation model (boolean / multivariate / percentage / rule-based / targeted); architecture (where evaluation runs — server, client SDK, edge); consistency vs latency tradeoff (strict freshness vs eventually-consistent SDK cache); audit and access control; circuit-breaker / kill-switch story; observability (which flag changed, who saw what variation, exposure metrics); dependencies between flags; testing strategy; rollback. 1500-3000 words.",
    acceptance: "docs/feature-flags.md exists; covers lifecycle, evaluation models including percentage and targeted, evaluation architecture (server/client/edge), consistency tradeoff, audit/RBAC, kill-switch, observability/exposure, flag dependencies, testing, rollback",
    verify: (cwd) => checkRubric(path.join(cwd, "docs/feature-flags.md"), {
      minWords: 1200,
      requiredSections: [
        /^#+\s.*(lifecycle|creation|cleanup)/im,
        /^#+\s.*(evaluation|model|variation|targeting)/im,
        /^#+\s.*(architecture|where|sdk|server|edge|component)/im,
        /^#+\s.*(consistency|cache|freshness|propag)/im,
        /^#+\s.*(audit|access control|permission|rbac)/im,
        /^#+\s.*(observab|metrics|exposure|monitoring)/im,
        /^#+\s.*(test|rollback|safety|kill)/im,
      ],
      requiredConcepts: [
        /percentage|gradual.*rollout|canary/i,
        /target(ed|ing)|cohort|segment/i,
        /sdk|client/i,
        /(eventually|stale|cache).*(consist|fresh)|(consist|fresh).*cache/i,
        /audit log|change log/i,
        /kill[- ]switch|panic button|emergency/i,
        /dependency|prerequisite|stacking/i,
      ],
    }),
  },
];

const TASK_SETS = { easy: EASY_TASKS, hard: HARD_TASKS, spec: SPEC_TASKS };
const TASKS = TASK_SETS[COMPARE_MODE] || EASY_TASKS;

function makeRepo() {
  const dir = mkdtempSync(path.join(tmpdir(), "compare-"));
  execSync("git init -q -b main && git config user.email c@c && git config user.name c", { cwd: dir, shell: "/bin/bash" });
  writeFileSync(path.join(dir, "README.md"), "compare\n");
  execSync("git add README.md && git commit -q -m init", { cwd: dir, shell: "/bin/bash" });
  return dir;
}

function extractClaudeUsage(messages) {
  for (const m of messages) {
    if (m.type === "result") {
      const u = m.usage || {};
      return {
        inputTokens: u.input_tokens || 0,
        outputTokens: u.output_tokens || 0,
        cacheReadTokens: u.cache_read_input_tokens || 0,
        cacheCreationTokens: u.cache_creation_input_tokens || 0,
        costUsd: m.total_cost_usd || 0,
        turnCount: m.num_turns || 0,
      };
    }
  }
  return { inputTokens: 0, outputTokens: 0, cacheReadTokens: 0, cacheCreationTokens: 0, costUsd: 0, turnCount: 0 };
}

function extractCodexUsage(repo) {
  const runsDir = path.join(repo, ".codex-dispatch", "runs");
  if (!existsSync(runsDir)) return null;
  const runs = readdirSync(runsDir);
  if (runs.length === 0) return null;
  let totalIn = 0, totalOut = 0, totalReasoning = 0, totalCacheIn = 0, turns = 0;
  for (const r of runs) {
    const stdoutPath = path.join(runsDir, r, "stdout.log");
    if (!existsSync(stdoutPath)) continue;
    const content = readFileSync(stdoutPath, "utf8");
    for (const line of content.split("\n")) {
      try {
        const obj = JSON.parse(line);
        if (obj.type === "turn.completed" && obj.usage) {
          totalIn += obj.usage.input_tokens || 0;
          totalOut += obj.usage.output_tokens || 0;
          totalReasoning += obj.usage.reasoning_output_tokens || 0;
          totalCacheIn += obj.usage.cached_input_tokens || 0;
          turns++;
        }
      } catch {}
    }
  }
  return { inputTokens: totalIn, outputTokens: totalOut, reasoningTokens: totalReasoning, cacheReadTokens: totalCacheIn, runCount: runs.length, turns };
}

async function collect(it) {
  const out = { messages: [], errored: null };
  try {
    for await (const msg of it) out.messages.push(msg);
  } catch (e) { out.errored = e; }
  return out;
}

async function runDirect(task) {
  const repo = makeRepo();
  const start = Date.now();
  const out = await collect(query({
    prompt: task.prompt,
    options: {
      cwd: repo,
      model: MODEL,
      maxTurns: 30,
      allowedTools: ["Read", "Write", "Edit", "Bash", "Glob", "Grep"],
      permissionMode: "bypassPermissions",
      settingSources: [],
    },
  }));
  const elapsedMs = Date.now() - start;
  const claude = extractClaudeUsage(out.messages);
  const verify = task.verify(repo);
  const deliverable = task.deliverableFile && existsSync(path.join(repo, task.deliverableFile))
    ? readFileSync(path.join(repo, task.deliverableFile), "utf8")
    : null;
  rmSync(repo, { recursive: true, force: true });
  return { errored: out.errored, claude, codex: null, elapsedMs, verify, deliverable };
}

async function runPlugin(task) {
  const repo = makeRepo();
  const start = Date.now();
  const prompt = `/codex-dispatch:codex --max-iter ${MAX_ITER} --no-tests --acceptance "${task.acceptance.replace(/"/g, '\\"')}" ${task.prompt}`;
  const out = await collect(query({
    prompt,
    options: {
      cwd: repo,
      model: MODEL,
      maxTurns: 30,
      plugins: [{ type: "local", path: PLUGIN_PATH }],
      permissionMode: "bypassPermissions",
      settingSources: [],
    },
  }));
  const elapsedMs = Date.now() - start;
  const claude = extractClaudeUsage(out.messages);
  const codex = extractCodexUsage(repo);
  const verify = task.verify(repo);
  const deliverable = task.deliverableFile && existsSync(path.join(repo, task.deliverableFile))
    ? readFileSync(path.join(repo, task.deliverableFile), "utf8")
    : null;
  if (process.env.KEEP_ON_FAIL && !verify.ok) {
    process.stderr.write(`    (kept ${repo} for inspection)\n`);
  } else {
    rmSync(repo, { recursive: true, force: true });
  }
  return { errored: out.errored, claude, codex, elapsedMs, verify, repo, deliverable };
}

// ─── LLM judge ─────────────────────────────────────────────────────────────
// Reads two specs blind (random A/B mapping per call) and scores them on a
// 5-dim 1-5 rubric, then picks a winner. Run multiple times for variance.

function lastJsonBlock(text) {
  let depth = 0, start = -1, end = -1;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (c === "{") { if (depth === 0) start = i; depth++; }
    else if (c === "}") { depth--; if (depth === 0) end = i; }
  }
  if (start === -1 || end === -1) return null;
  try { return JSON.parse(text.slice(start, end + 1)); } catch { return null; }
}

function assistantTextOnly(messages) {
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

const JUDGE_PROMPT = (taskDesc, specA, specB) => `You are evaluating two design specifications written by different agents for the same task. Score each on a 5-dim rubric and pick a winner. Do NOT favor either based on length, format, or stylistic conventions — judge substance.

DIMENSIONS (1-5 each, integer):
- depth: depth of analysis vs. surface enumeration. 5 = identifies non-obvious constraints, gives concrete numbers, cites real failure modes.
- clarity: structure, readability, signal-to-noise. 5 = scannable, headers track the actual content, no padding.
- tradeoff_articulation: explicitly weighs alternatives instead of asserting a choice. 5 = at least one decision is defended against named alternatives with crisp pros/cons.
- completeness: covers the actual problem space asked. 5 = no major gap a reader would notice; edge cases acknowledged.
- open_questions: honest about unknowns. 5 = lists real, decision-blocking unknowns rather than padding with "what should we do about X".

TASK
${taskDesc}

==== SPEC A ====
${specA}

==== SPEC B ====
${specB}

Reply with EXACTLY this JSON object as the last thing in your response (no markdown fence around it):
{"scores_A":{"depth":N,"clarity":N,"tradeoff_articulation":N,"completeness":N,"open_questions":N},"scores_B":{"depth":N,"clarity":N,"tradeoff_articulation":N,"completeness":N,"open_questions":N},"winner":"A|B|tie","reasoning":"one sentence — what concretely tipped the call"}`;

async function callJudgeClaude(prompt) {
  const out = await collect(query({
    prompt,
    options: {
      model: JUDGE_MODEL,
      maxTurns: 2,
      // Judge does no tool work — just reads + scores.
      allowedTools: [],
      settingSources: [],
    },
  }));
  if (out.errored) return { errored: out.errored };
  const text = assistantTextOnly(out.messages);
  const usage = extractClaudeUsage(out.messages);
  return { text, costUsd: usage.costUsd };
}

async function callJudgeSynthetic(prompt) {
  if (!SYNTHETIC_KEY) {
    return { errored: new Error("SYNTHETIC_API_KEY not set") };
  }
  const body = {
    model: JUDGE_MODEL,
    messages: [
      // System message keeps the judge focused. DeepSeek V3.2 is a reasoning
      // model and likes to think out loud, so we give it room AND ask for
      // the JSON last so a truncated response still contains it.
      { role: "system", content: "You are a strict, impartial evaluator. Score the two specs on the rubric. End your response with the requested JSON object on its own — no markdown fences, no commentary after the closing brace." },
      { role: "user", content: prompt },
    ],
    max_tokens: 4000,
    temperature: 0,
  };
  let resp;
  try {
    resp = await fetch(`${SYNTHETIC_BASE}/chat/completions`, {
      method: "POST",
      headers: {
        "Authorization": `Bearer ${SYNTHETIC_KEY}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify(body),
    });
  } catch (e) {
    return { errored: new Error(`fetch failed: ${e.message}`) };
  }
  if (!resp.ok) {
    const errText = await resp.text().catch(() => "");
    return { errored: new Error(`synthetic ${resp.status}: ${errText.slice(0, 300)}`) };
  }
  const json = await resp.json();
  const text = json?.choices?.[0]?.message?.content || "";
  const usage = json?.usage || {};
  const price = SYNTHETIC_PRICES[JUDGE_MODEL];
  const costUsd = price
    ? ((usage.prompt_tokens || 0) * price.input + (usage.completion_tokens || 0) * price.output) / 1e6
    : 0;
  return { text, costUsd };
}

async function judgeOnce(task, directDeliverable, pluginDeliverable) {
  // Random blind: flip A/B labels per call so the judge can't infer source from order.
  const flip = Math.random() < 0.5;
  const specA = flip ? pluginDeliverable : directDeliverable;
  const specB = flip ? directDeliverable : pluginDeliverable;
  const aScenario = flip ? "plugin" : "direct";
  const bScenario = flip ? "direct" : "plugin";

  const prompt = JUDGE_PROMPT(task.prompt, specA, specB);
  const result = JUDGE_PROVIDER === "synthetic"
    ? await callJudgeSynthetic(prompt)
    : await callJudgeClaude(prompt);
  if (result.errored) return { errored: result.errored };

  const obj = lastJsonBlock(result.text);
  if (!obj || !obj.winner || !obj.scores_A || !obj.scores_B) {
    return { errored: new Error(`unparseable judge output: ${result.text.slice(-300)}`) };
  }
  const directScores = flip ? obj.scores_B : obj.scores_A;
  const pluginScores = flip ? obj.scores_A : obj.scores_B;
  const winnerScenario = obj.winner === "A" ? aScenario
                       : obj.winner === "B" ? bScenario
                       : "tie";
  return {
    winner: winnerScenario,
    directScores,
    pluginScores,
    reasoning: obj.reasoning,
    usage: { costUsd: result.costUsd },
    flipped: flip,
  };
}

function meanScores(scoresList) {
  const dims = ["depth", "clarity", "tradeoff_articulation", "completeness", "open_questions"];
  const result = {};
  let total = 0;
  for (const d of dims) {
    const sum = scoresList.reduce((a, s) => a + (s[d] || 0), 0);
    result[d] = scoresList.length ? +(sum / scoresList.length).toFixed(2) : 0;
    total += result[d];
  }
  result.total = +total.toFixed(2);
  return result;
}

async function judgePair(task, direct, plugin) {
  if (!direct.deliverable || !plugin.deliverable) {
    return { skipped: true, reason: "deliverable missing" };
  }
  const runs = [];
  let totalUsd = 0;
  for (let i = 0; i < JUDGE_RUNS; i++) {
    process.stderr.write(`    judge run ${i + 1}/${JUDGE_RUNS}... `);
    const r = await judgeOnce(task, direct.deliverable, plugin.deliverable);
    if (r.errored) {
      process.stderr.write(`ERR (${r.errored.message?.slice(0, 80) || ""})\n`);
      continue;
    }
    runs.push(r);
    totalUsd += r.usage?.costUsd || 0;
    process.stderr.write(`${r.winner} (D=${Object.values(r.directScores).reduce((a,b)=>a+b,0)} P=${Object.values(r.pluginScores).reduce((a,b)=>a+b,0)})\n`);
  }
  if (runs.length === 0) return { skipped: true, reason: "all judge runs errored" };
  const wins = { direct: 0, plugin: 0, tie: 0 };
  for (const r of runs) wins[r.winner]++;
  const directMean = meanScores(runs.map(r => r.directScores));
  const pluginMean = meanScores(runs.map(r => r.pluginScores));
  return { runs, wins, directMean, pluginMean, totalUsd, sampleReasoning: runs[0]?.reasoning || "" };
}

function fmt(n, w) { return String(n).padStart(w); }
function fmtUsd(n) { return `$${n.toFixed(4)}`; }

function row(task, scenario, r) {
  const c = r.claude;
  const x = r.codex;
  const status = r.errored ? "ERR" : (r.verify.ok ? "ok" : "FAIL");
  const totalTokens = c.inputTokens + c.outputTokens + (x ? x.inputTokens + x.outputTokens : 0);
  return [
    task.name.padEnd(8),
    scenario.padEnd(6),
    status.padEnd(4),
    `${(r.elapsedMs/1000).toFixed(1)}s`.padStart(7),
    fmt(c.turnCount, 4),
    fmt(c.inputTokens, 7),
    fmt(c.outputTokens, 7),
    fmt(c.cacheReadTokens, 8),
    x ? fmt(x.turns, 4) : "—".padStart(4),
    x ? fmt(x.inputTokens, 7) : "—".padStart(7),
    x ? fmt(x.outputTokens, 7) : "—".padStart(7),
    fmt(totalTokens, 8),
    fmtUsd(c.costUsd).padStart(8),
  ].join("  ");
}

async function main() {
  console.log(`Direct vs codex-dispatch plugin — model: ${MODEL}`);
  console.log("");
  const header = ["task    ", "scen  ", "stat", "   time", "cTrn", "claudeIn", "claudeOu", "claudeCac", "xTrn", "codexIn", "codexOu", "totalTok", "  costUSD"].join("  ");
  console.log(header);
  console.log("─".repeat(header.length));

  const summary = [];
  for (const task of TASKS) {
    if (process.argv.length > 2 && !process.argv.slice(2).some(f => task.name.includes(f))) continue;
    process.stderr.write(`\n  → ${task.name} (direct)... `);
    const direct = await runDirect(task);
    process.stderr.write(`${direct.errored ? "ERR" : direct.verify.ok ? "ok" : "FAIL"} (${(direct.elapsedMs/1000).toFixed(1)}s)\n`);
    process.stderr.write(`  → ${task.name} (plugin)... `);
    const plugin = await runPlugin(task);
    process.stderr.write(`${plugin.errored ? "ERR" : plugin.verify.ok ? "ok" : "FAIL"} (${(plugin.elapsedMs/1000).toFixed(1)}s)\n`);

    console.log(row(task, "direct", direct));
    console.log(row(task, "plugin", plugin));
    if (direct.errored) console.log(`  ! direct error: ${direct.errored.message}`);
    if (!direct.verify.ok && !direct.errored) console.log(`  ! direct verify: ${direct.verify.reason}`);
    if (plugin.errored) console.log(`  ! plugin error: ${plugin.errored.message}`);
    if (!plugin.verify.ok && !plugin.errored) console.log(`  ! plugin verify: ${plugin.verify.reason}`);
    let judgeResult = null;
    if (JUDGE_RUNS > 0 && direct.verify.ok && plugin.verify.ok && task.deliverableFile) {
      process.stderr.write(`  → ${task.name} judging (${JUDGE_RUNS} runs)...\n`);
      judgeResult = await judgePair(task, direct, plugin);
      if (judgeResult.skipped) {
        console.log(`  ! judge skipped: ${judgeResult.reason}`);
      } else {
        const w = judgeResult.wins;
        console.log(`  judge: direct=${w.direct} plugin=${w.plugin} tie=${w.tie}  | direct mean=${judgeResult.directMean.total}/25 plugin mean=${judgeResult.pluginMean.total}/25  | judge cost ~${fmtUsd(judgeResult.totalUsd)}`);
        console.log(`         sample reasoning: "${judgeResult.sampleReasoning.slice(0, 140)}"`);
      }
    }
    summary.push({ task: task.name, direct, plugin, judge: judgeResult });
  }

  console.log("");
  console.log("Summary:");
  for (const s of summary) {
    const d = s.direct, p = s.plugin;
    const dTok = d.claude.inputTokens + d.claude.outputTokens;
    const pClaudeTok = p.claude.inputTokens + p.claude.outputTokens;
    const pCodexTok = p.codex ? p.codex.inputTokens + p.codex.outputTokens : 0;
    const pTok = pClaudeTok + pCodexTok;
    const tokenRatio = dTok > 0 ? (pTok / dTok).toFixed(1) : "n/a";
    const timeRatio = d.elapsedMs > 0 ? (p.elapsedMs / d.elapsedMs).toFixed(1) : "n/a";
    console.log(`  ${s.task.padEnd(8)} direct ${d.verify.ok ? "ok " : "FAIL"}, plugin ${p.verify.ok ? "ok " : "FAIL"} | tokens: direct=${dTok} plugin=${pTok} (${tokenRatio}x) | time: direct=${(d.elapsedMs/1000).toFixed(1)}s plugin=${(p.elapsedMs/1000).toFixed(1)}s (${timeRatio}x)`);
  }
}

main().catch((e) => { console.error("crashed:", e); process.exit(2); });
