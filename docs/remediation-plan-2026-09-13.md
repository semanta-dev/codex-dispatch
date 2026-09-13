# Full remediation plan — 2026-09-13

Status: **user-approved; implementation in progress.**
Approval covers remediation and verification; it does not authorize release,
merge, push, or deployment. Current evidence is tracked in the
[qualification matrix](qualification-matrix-2026-09-13.md).
Release remains **NO-GO**. Scope includes five findings in the September 13
review and the outstanding F01–F12 qualification obligations in
[remediation progress](remediation-progress.md) and the
[existing CTO plan](cto-remediation-2026-09-09.md).

## Evidence and scope

Planning baseline is `b897a43` plus four existing modifications:
`scripts/owned_process.py`, `tests/python/test_execution_policy.py`,
`tests/native/windows-sandbox-probe/main_windows.go`, and
`docs/windows-verification-design.md`. Preserve and reconcile those changes;
do not reset them or silently count them as a clean qualification candidate.
The final release candidate will be frozen after implementation.

The prior review needs two precision corrections: artifact disclosure depends
on the effective umask/ACL and ancestor access, and archive escape was not
reproduced against the installed tar/unzip versions. The extraction fallback
lacks an application-enforced member policy; platform tools may independently
reject attacks. Phase 0 will establish the exact exposure and retain rejected
attacks as controls. Native NO-GO is existing qualification debt, not a newly
demonstrated critical exploit. The Python baseline was 135 tests run, 11 skipped
(124 passed), not 135 passed plus 11 skipped. The Go race suite passed.

| ID | Finding and evidence | Closure work |
| --- | --- | --- |
| R1 | Synchronous `result.json` symlink overwrote an external sentinel while dispatch returned success | Packets 1, 5, 6 |
| R2 | Artifact producers request 0755 directories and 0644 files; effective access needs account/ACL controls | Packets 1, 6 |
| R3 | Dirty tracked file produced a WIP-relative patch that failed against HEAD in `--clean-verify` | Packets 2, 5, 6 |
| R4 | Deleting a tracked parent directory caused snapshot `FileNotFoundError` | Packets 2, 5, 6 |
| R5 | Launcher falls back to whole-archive extraction without validating all member types/paths | Packets 3, 6 |
| R6 | Native confinement/lifecycle, toolchain inputs, public routes and promotion evidence remain incomplete | Packets 4–6; all existing F01–F12 gates |

Also retain the observed successful verifier that creates ignored `build/result`
but is reported as mutating reviewed source. Packet 4 must support legitimate
build outputs through an explicit policy without broadly excluding source.

## Boundaries that must hold

Treat dispatched code, repository paths, verifier output, links/special files,
and archive bytes as untrusted. Protect host files, source/WIP, reviewer
credentials, authoritative evidence, cache publication and descendant cleanup.

Permissions protect against other accounts; they do not isolate a same-UID
child. Atomic replacement prevents truncating a linked inode; it does not by
itself authenticate evidence. Authoritative control artifacts must be outside
child-writable mounts, with protected controller ownership and pinned handles.
Workspace-visible reports are exports, never the source of completion authority.
Tests must demonstrate this distinction. An arbitrary unconfined same-UID
process or compromised kernel is outside the promised containment boundary;
do not imply 0700, hashing, or a random pathname defeats that attacker.

Keep fail-closed behavior, task-only attribution, finite deadlines and the
serialized checkout contract. Do not widen HOME/network/filesystem permissions
to make positive tests pass. Checksums detect corruption; a compromised release
publisher can ship a malicious binary regardless of archive validation.

## Work packets and exit gates

Owners are implementation/review roles, not staffing commitments. Codex will
coordinate execution after approval, using file claims and independent reviews.

### 0. Establish reproductions and the closure ledger

**Owner:** remediation lead + security reviewer. **Dependency:** approval.

The user approved implementation after the planning and persona-review rounds.

Record the exact working-tree manifest and distinguish inherited changes from
new work. Turn R1/R3/R4 reproductions into deterministic regressions in temporary
repositories with fake Codex, external sentinels and no real secrets. Characterize
R2 with permissive/restrictive umasks and a second account; characterize R5 with
tar/zip attacks on the actual native extraction tools. Correct findings without
dropping the prevention requirements when a native tool already rejects input.

Inventory all artifact writers/readers: sync, cancel, detached, broker logs/task
store, baseline/diff, Python collector, receipts/review archive, plan cache and
installer staging. Link each to an owner, regression and public entrypoint.
Reconcile the newer Windows observation with the progress document.

**Exit:** every finding has a trigger, expected behavior and named gate; existing
F01–F12 identifiers and historical evidence remain intact.

### 1. Protect artifact authority, writes and confidentiality

**Owner:** runtime/evidence engineer; security reviewer accepts the boundary.
**Dependency:** packet 0. **Primary files:** `internal/dispatch`, `internal/diff`,
`internal/result`, `internal/broker`, Python evidence/controllers and native
file-policy helpers.

Introduce a narrowly scoped artifact store contract shared by Go/Python
consumers. Create random, exclusive run directories with 0700 permissions and
0600 files on POSIX; use explicit owner-only Windows DACLs at creation. Pin
directory and reader handles. Mutable leaves use fresh exclusive temporary
files, flushed content and atomic handle-relative replacement; create-only
identities must never be replaced. Do not truncate through symlinks or hardlinks.
Cover parent substitution, reparse points, FIFOs, partial writes and cancellation.

Version the private authority format and bind each run to its baseline tree,
index digest, task-patch digest, candidate fingerprint, effective workdir and
terminal state. Verify those bindings before review and acceptance. Preserve
the public `result.json` ordering, shape and exit behavior, or version its
consumer migration.

Keep authoritative baselines, receipts, results and reviewer records in a
controller-owned store excluded from dispatched/verification writable mounts.
Carry immutable run identity through the controller/broker and resolve final
validation from that authority. Preserve workspace run paths as inspectable
exports. Do not trust model-written copies or hashes stored alongside them.
Define broker lifetime, restart/recovery and cleanup behavior before migration.

Store raw WIP blobs/trees and index bytes in a private controller-owned store,
not ordinary `.git/objects`; deny child writes and test access from a second
account. Keep recovery until explicit cleanup, never auto-prune or rewrite
history, and document that deletion is not physical erasure. Inventory legacy
repository objects as potentially exposed and provide opt-in migration.

For `CODEX_RESULT_DIR`, preserve a safe exact directory as the export destination;
reject unsafe or active reuse with an actionable diagnostic. Never chmod a user's
arbitrary parent directory. Existing artifacts are historical/untrusted until
validated; do not recursively chmod, follow links or silently promote them.
If native isolation cannot exclude the private store, the reviewed route remains
unsupported rather than falling back to mutable workspace evidence.

**Exit:** malicious leaf/parent/link replacements cannot overwrite an external
sentinel or grant forged PASS; readers reject changed authority; concurrent runs
cannot collide; errors/cancellation persist atomic terminal outcomes; a separate
account cannot read new private artifacts. Test both export and authority paths
through sync, detached, failed and resumed public flows on supported platforms.
The packet security reviewer signs these boundary tests; final qualification is
performed independently of the implementation author.

### 2. Make clean verification and deletions match the captured baseline

**Owner:** evidence engineer. **Dependency:** packet 1's baseline contract.
**Primary files:** `internal/diff/snapshot.go`, `scripts/clean_verify.py`,
`scripts/execution_policy.py`, related Go/Python/Bats tests.

Use the captured version-2 baseline tree as clean materialization input, then
apply the task-only patch. Validate version, recorded HEAD/tree/ref and run
identity against controller-owned evidence. Read raw objects without hooks,
filters, replacement objects, credential inheritance or lazy fetch. Retain WIP
recovery data in the private store and original index bytes; never reset/stage
the user's checkout. Materialize using the protected authority record.
Legacy HEAD-only records cannot qualify for new reviewed completion: return an
actionable unsupported-record error rather than guessing the baseline.

Treat an absent tracked parent as a deletion at snapshot time. Catch only genuine
missing-path cases; symlink traversal, permission errors and conflicting state
remain structured failures. Detect concurrent source/index drift before accepting
the materialization and at final acceptance; preserve user edits on rejection.

**Exit:** staged+unstaged WIP, untracked baseline inputs, file/directory deletions,
renames, binaries, modes, symlinks and module scoping round-trip correctly.
Include additions subsequently ignored by task edits: baseline-plus-patch must
match the promised reviewed state. Corrupt/missing/substituted records, unsafe
parents and concurrent changes fail without hangs or changes to WIP/index.

### 3. Validate archives and publish the cache atomically

**Owner:** release/tooling engineer. **Dependency:** packet 0; can run alongside 1.
**Primary files:** `scripts/dispatch-codex.sh`, launcher helpers/tests, packaging.

Replace whole-archive fallback with a bounded parser/extractor that accepts one
exact platform binary as a regular member. Define the actual GoReleaser metadata
allowlist; validate the entire inventory before writing any member. Reject
duplicates/collisions, links, devices/FIFOs, traversal/absolute paths and Windows
drive/UNC/alternate-stream or case-folding ambiguities. Bound archive size,
expanded size, member count, time and memory; exercise both valid tar and zip.

Ship the parser with the plugin using the existing Python 3 prerequisite; pin
and document its supported minimum version and preflight it on cold installs.
It must not depend on the binary being downloaded, a Go toolchain or a network
package install. Test bootstrap with only documented prerequisites available.

Extract to private staging and publish a fresh exclusive file atomically under
the existing cache lock. Apply the same rules to manual/offline installation,
including concurrency and interruption. Keep version, checksum and platform
binding. Do not weaken checks or fall back to an older release on failure.

**Exit:** hostile archive corpora leave no outside writes or partial installed
binary; valid packaged candidates install with/without flock, concurrently,
offline and after interruption. Test launcher resolution without binary overrides.

### 4. Complete native execution and usable toolchain policies

**Owner:** platform/runtime engineers + security reviewer.
**Dependency:** diagnostics may begin after 0; production integration uses 1–2.
**Primary files:** execution/ownership/native helpers, native probes, CI, platform
design documents and explicit verification-input configuration.

Begin with a bounded feasibility checkpoint of up to five engineer-days. Report
observations and revised estimates; this is not a five-day delivery promise.
Linux: reproduce the amd64 timing failure with retained diagnostics and the
existing bounds; qualify amd64/arm64 without global AppArmor relaxation.
Windows: establish actual Go and Git Bash/MSYS startup/I/O under the constrained
token plus creation-time Job ownership and descendant termination. macOS:
establish shell/Go startup, narrow filesystem/network policy and reliable
descendant ownership. Only integrate a backend after positive, denial and
lifecycle controls pass. If feasibility fails, report the blocker and options;
the six-platform target remains unchanged without a separate user decision.

Define explicit pinned toolchain/dependency inputs and disposable output/cache
locations, separate from reviewed source. Demonstrate real Go, Python, Node and
Rust checks where advertised. Do not inherit host HOME, credentials or broad
dependency caches. Permit declared generated output without letting .gitignore
changes or broad directory exclusions hide source drift. Enforce process, file,
memory, output and wall-time budgets with native equivalents.

**Exit:** legitimate builds/tests pass; fake-secret, outside-write, network and
artifact-tampering controls fail; forked/detached descendants cannot write after
timeout, cancellation, repeated interruption or supervisor death. Unsupported
backends continue to reject execution and cannot produce accepted evidence.

### 5. Exercise all product routes and migration behavior

**Owner:** integration engineer. **Dependency:** 1–4.

Run direct API review, supported CLI controller, delegated simple tasks,
orchestration, GraphRAG packet/plan in both isolation modes, module workdirs,
clean verification, resume/fresh fallback and cold background handoff. Confirm
identity, tests, iteration caps, failure accounting, presentation and artifact
authority survive each wrapper. Test in an actual Claude session where UI/route
behavior cannot be established by a fake transport. Legacy routes stay explicitly
unqualified until they satisfy the contract.

**Exit:** stale/forged/drifting evidence never grants PASS; failed/no-op/cancelled
attempts retain their complete history; cached packets are validated; checkout
serialization and injected-child-failure checks remain effective. Publish the
capability/migration matrix and verify documented commands match behavior.

### 6. Freeze, independently qualify and prepare release

**Owner:** release owner; independent security and CTO reviewers.
**Dependency:** 1–5 complete and no unresolved high-severity findings.

Implement the missing promotion-evidence producer from real command/run records;
synthetic records remain rejection tests only. Freeze commit/tree, configuration,
tool versions and artifact digests. Candidate changes invalidate affected proof;
never assemble a release matrix from different executable candidates.

Retain all six native platforms, F01–F12 and explicit R1–R5 regression coverage,
positive/lifecycle controls and cold installation of the selected archives.
Run nine reviewer fixtures with ten observations each (minimum 8/10 each), then a
fresh 30+30 paired cohort: plugin accepted >=27/30 and no lower than direct,
zero critical violations, latency and all-spend per accepted task ratios <=1.25.
Retain failures/retries and complete cost/profile/transport evidence. No prompt
tuning, omitted failures, weakened thresholds or historical proof substitution.

**Exit:** independent security and CTO decisions address the exact candidate;
the evidence assembler/gate reject altered, missing, stale or self-approved
records. Verify protected-environment and external digest requirements. Prepare
the exact six approved assets and a rollback/withdrawal procedure that preserves
fail-closed behavior. Publication requires a separate user release instruction;
after publication, verify the actual cold-download route before expanding use.

## Execution and approval boundaries

Packets 1 and 3 can run in parallel after 0; native diagnostics can overlap with
them. Packet 2 follows the authoritative baseline design; integration and final
qualification follow their prerequisites. Keep changes in focused commits/PRs,
with Semanta context before each packet, claims before edits, regression evidence
at completion and durable handoffs. No indexed codex-dispatch project was returned
by Semanta; this plan is grounded in local code plus the existing user-scoped
history rather than an invented project ID or inferred graph coverage.

Implementation approval covers this remediation scope, local tests and available
native CI preparation/execution. Before live paid fixture/benchmark runs, present
a concrete estimated cost and ask for a spending cap if one is not already
authorized. No approval here authorizes publishing, merging, changing production
credentials, broad host security exceptions or reducing the platform contract.

Native feasibility and external qualification prevent a defensible total delivery
date today. The first checkpoint is packet 0 plus a scoped implementation estimate
for 1–3; the native checkpoint supplies the remaining estimate. Stop and reassess
if the evidence boundary cannot be enforced or native feasibility requires a
material architecture/scope change. Rollback disables the affected route; it does
not reinstate unsafe behavior.

## Independent review record

Round 1: security required authority isolation beyond chmod/atomic writes,
bounded member extraction and explicit threat limits. CTO required version-2
baseline semantics, safe deletion handling, complete same-candidate qualification,
preservation of WIP and a feasibility checkpoint rather than a native promise.
Round 2: CTO approved the sequence and requested clearer approval wording.
Security required private raw WIP/object/index storage, explicit authority
bindings, result-format compatibility and a parser usable during cold bootstrap.
Those changes are incorporated above; final confirmation is pending. Persona
approval means approval of the plan, not implementation qualification or release
authorization.
