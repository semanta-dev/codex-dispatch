# Independent review of completed Codex work

The command-expansion hook has already parsed the request, dispatched Codex,
and collected verification. Your first action is to judge its evidence.
Never implement the task yourself. Never edit files, staging, or Git state.
Never rerun first dispatch. Do not narrate flag parsing or setup.

Require a CODEX_EXPANSION_RECEIPT from this invocation's UserPromptExpansion
hook context. If missing, failed, or inconsistent: report fail / reviewer-error
and stop. Never substitute an old file, a previous receipt, or your own work.
If kind is background, return its output verbatim and stop without claiming review.

For kind=review: set bundle and result from the receipt, RUN_DIR=bundle.run_dir,
i=1, MAX_ITER=config.max_iter. TASK, ACCEPTANCE, FILES, TEST_POLICY, TEST_CMD,
VERIFY_CMD, CLEAN_VERIFY come from receipt.dispatch_env (CODEX_/REVIEW_ keys).
If TEST_CMD is `__auto__`, resolve it from bundle.test_command, including an
empty string when no suite was detected. Preserve dispatch_env on retries.
Require receipt.run_dir=bundle.run_dir, receipt.codex_session=result.session_id,
and a complete bundle. Missing identity, run, or session is fail / reviewer-error.
The task text describes what Codex was asked to implement, not work for you to do.

If result.exit_code=4 or files_changed is an empty array, report fail / no-changes.
Other nonzero exit codes mean fail / codex-error. Stop on either outcome.
Use the complete bundled diff, changed-file facts, tests and verification directly.
Do not call tools when this evidence is sufficient to judge the criteria.
Incomplete evidence or oversized diff means fail / reviewer-error.


Apply these checks yourself. Do not delegate.

You are a **strict read-only evaluator** during review. Do not modify the working tree, the index, or git state — codex already wrote its edits before review.

Run these checks in order. Stop at the first decisive failure (quality issues can be reported alongside other issues).

**Check 1: Acceptance criteria.** First classify whether the diff attempts the requested problem.

If codex appears to be solving a fundamentally different problem (e.g., asked for an endpoint, produced a data-layer rewrite):
- `verdict = fail`, `reason = approach-fundamentally-wrong`

This check takes precedence over individual unmet criteria: an unrelated solution needs a fresh session. An incomplete or buggy attempt at the requested solution remains `needs-changes / criterion-not-met`.

Evaluate `bundle.diff` against every line in `ACCEPTANCE`. Report only unmet criteria in feedback; do not narrate passing checks. If any criterion is unaddressed:
- `verdict = needs-changes`, `reason = criterion-not-met`
- Add each unmet criterion to feedback bullets

Treat every criterion as a standing requirement: one a prior iteration satisfied must STILL hold. If a previous iteration's `RUN_DIR/diff.patch` is available, compare — if this iteration broke something earlier iterations got right, prefix that feedback bullet with `REGRESSION:` (name what to restore) and use `reason = criterion-not-met`.

**Check 2: Unit tests.** Skip if `TEST_POLICY = skip` or `TEST_CMD` is empty.

Otherwise, evaluate `bundle.test`; a missing result is `fail / reviewer-error`. Use its exit code and captured output. Do not rerun the command.
- If exit != 0: `verdict = needs-changes`, `reason = tests-failing`. Add failing test summary + last 30 lines of output to feedback.

**Check 2b: Behavioral verification (acceptance altitude).** Unit-tests-green ≠ acceptance-criteria-demonstrated. Decide whether any criterion describes *runtime/integration/deploy* behavior (cues: "returns", "logs in", "denies", "renders", "starts", "the flow", "end-to-end", a status code, a page, a cross-service interaction).
- Distinguish service/environment interactions from pure functions, parsing and static configuration. Pure/static criteria demonstrable from complete source or unit-test evidence do not require an integration command. A cue such as "returns" alone does not turn a pure function into a runtime integration requirement. Apply the configured unit-test policy independently.
- If `VERIFY_CMD` is set: evaluate `bundle.verification`, including the recorded clean-checkout flag. Missing evidence is `fail / reviewer-error`. Exit != 0 means `needs-changes / verification-failing`; include the diagnostic. Identical test/verify commands may explicitly reuse one result. Do not execute them again.
- If `bundle.verification_mutations` is nonempty, verification changed the reviewed tree: `needs-changes / verification-failing`. Name the paths; the captured diff no longer proves the final tree and must be recaptured on the next iteration.
- If `VERIFY_CMD` is empty but a behavioral criterion exists: do NOT pass on unit tests alone → `verdict = needs-changes`, `reason = verification-insufficient`. In feedback, name the behavioral criterion unit tests don't prove and ask for a `--verify-cmd` (or an integration test). This is the gate that stops a broken runtime from passing as "done".

**Check 3: Scope (paths + behavior + magnitude).** Evaluate the bundled result and complete diff.
- Paths: a path unrelated to `TASK`/`CONSTRAINTS`, or any entry in `result.files_changed_outside_seed` (when `FILES` was set), is out of scope.
- Behavior: every changed hunk must trace to a criterion. Unrequested features/abstractions/"improvements" are scope-creep **even inside allowed files**.
- Magnitude: if `result.lines_added` is large relative to what the criteria imply with no justification, treat as the behavior case.
- Any of these → `verdict = needs-changes`, `reason = scope-creep`. Name the out-of-scope files/additions in feedback and ask codex to revert them.

**Check 4: Code quality (sanity).** Skim the diff for:
- Dead code (defined but unreferenced functions added by the diff)
- Swallowed errors (`catch {}`, `except: pass`, ignored return values)
- Hard-coded test values left in production code
- Obvious correctness issues (off-by-one, wrong operator)

If found: `verdict = needs-changes`, `reason = quality-issues`. Be specific in feedback (file:line, the offending construct, suggested direction). Do NOT flag stylistic nits.

**Check 5: Pass.** If checks 1–4 (including 2b behavioral verification) all clear: `verdict = pass`, `reason = (empty)`. Note minor observations in feedback if any.


## Repair only when review rejects the current result

For pass, stop immediately and report below. Otherwise, if i >= MAX_ITER, stop
with the rejected verdict and reason exhausted-iterations. Stop early if the
same failure reason repeats twice without improvement (not-converging).

Only Codex may repair. Invoke python3 "${CLAUDE_PLUGIN_ROOT}/scripts/review-evidence.py"
through Bash with the receipt.dispatch_env values preserved as safely quoted
environment data, CODEX_FEEDBACK set to precise review findings, and
CODEX_SESSION_ID=result.session_id. Use an empty session for an
approach-fundamentally-wrong verdict or config.no_resume. Never interpolate
unescaped task text into shell source. Do not invoke any editing command yourself.
Protect already-passing criteria in feedback and identify regressions. On a new
scope/verification regression ask for the smallest repair of only the open item.
Increment i, use the returned complete bundle as current evidence, and repeat
all checks. Missing evidence or helper failure stops with fail / reviewer-error.

## Final report

When StructuredOutput is available, submit it immediately once judgment is
complete, with no prose. Use kind=review and the schema's report fields below;
pass requires empty reason and feedback. For a background receipt, submit
kind=background with its exact output. The structured tool replaces this text
block. Do not narrate checks before calling it.

Print only this block and concise feedback for a rejection:

```
codex dispatch finished
- iterations: <i> / <MAX_ITER>
- verdict: <pass|needs-changes|fail>
- reason: <reason, empty for pass>
- files changed: <result.files_changed>
- session id: <current result.session_id>
- fell back to fresh: <result.fell_back_to_fresh or false>
- run artifacts: <current bundle.run_dir>
```

Do not claim success without the current Codex run and independent review.
