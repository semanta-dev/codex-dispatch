# Native Windows confined verification design

Status: feasibility prototype, September 9, 2026. The fixed-command experiment
in `tests/native/windows-sandbox-probe/main_windows.go` cross-compiles for Windows
amd64 and arm64. Its manual workflow, also triggered by probe changes on `codex/plan-a-repair`, is
`.github/workflows/windows-sandbox-probe.yml`. Native execution has not yet been
performed. There is no production Windows backend; Windows verification remains
unsupported and cannot satisfy promotion gates yet.

The prototype records pre-execution LPAC/job identity, cmd and Git Bash positive
controls, and native child denial of fake outside credentials, outside writes,
and loopback connections. `PROBE-PASS` means only those fixed controls passed,
not production readiness. It now includes forced supervisor termination with independently opened handles
for two ready descendants, absence of their delayed writes, and a suspended
`CREATE_BREAKAWAY_FROM_JOB` attempt that must fail with access denied. These
controls are implemented but still require native execution. Arbitrary verifier
compatibility and full public route conformance remain outside the prototype.

As part of Plan A (Repair), add a dedicated native Go verification launcher using a
less-privileged AppContainer (LPAC), empty capabilities, explicit ACLs, and a
Job Object established before any verification code executes.

## Runtime prerequisites and boundary

Use Windows 10/11 or Server 2016+ APIs. `STARTUPINFOEX` carries
`PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES` with a unique AppContainer SID
and zero capabilities. Add
`PROC_THREAD_ATTRIBUTE_ALL_APPLICATION_PACKAGES_POLICY` with
`PROCESS_CREATION_ALL_APPLICATION_PACKAGES_OPT_OUT` for LPAC. Ordinary
AppContainer grants broader implicit access than this design intends.

Create a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`. Set neither
breakaway flag. Prefer creation-time `PROC_THREAD_ATTRIBUTE_JOB_LIST`, or use
`CREATE_SUSPENDED`, assign successfully, then resume. The existing appserver
controller assigns its job after process start and must not be copied unchanged
for this boundary. On any setup failure, terminate the suspended process.

Only the trusted supervisor owns the noninheritable job handle. Whitelist
inherited stdin/stdout/stderr handles; do not inherit arbitrary host handles.
Construct the Unicode environment explicitly and exclude credentials and
interpreter/Git startup configuration.

## Disposable filesystem and execution

Create a unique profile with `CreateAppContainerProfile`; include its profile
storage in explicitly allowed scratch and clean it with
`DeleteAppContainerProfile`. Give the package SID write access only to the
snapshot and scratch. Protect DACLs from broad inherited permissions and apply
appropriate low-integrity labels to writable objects. Grant read/execute access
to a pinned copied portable Git/toolchain layout rather than changing host
installation ACLs. Reject reparse-point escapes during materialization/audit.

Launch absolute Git Bash with `--noprofile --norc -c`. Explicitly set HOME,
USERPROFILE, TEMP, TMP, LOCALAPPDATA, and PATH. Supervise output through bounded
pipes, apply a wall deadline and job resource limits, drain the complete job,
and only then audit snapshot mutations. Preserve the existing
`execution_policy.verify` result contract; do not treat `taskkill` as isolation.

Git Bash/MSYS compatibility is an experiment, not a proven property. Its DLL
loading, fork emulation, named objects, `/tmp`, and registry accesses need native
probes. LPAC does not normally permit registry/COM access without capabilities.
Diagnose exact denied resources before adding a narrowly justified grant. Broad
user-profile, registry, COM, network, or installation write access is not an
acceptable workaround. If necessary, select a different Windows execution
boundary rather than weakening these constraints.

## Native probe workflow

Build and run a small Go probe on `windows-latest` and `windows-11-arm`.
Record actual process architecture, token AppContainer status, package SID,
capabilities, and job membership. ARM64 cross-compilation alone is insufficient.
The hosted ARM64 image lists Git and Bash, but their actual architecture and
LPAC behavior still need measurement.

1. Positive control: Bash starts, creates children, reads source, runs a small Go
   test, and writes allowed scratch with the correct module cwd.
2. Negative controls: fake credential read outside the snapshot, outside write,
   host-process dangerous handle access, loopback connection, and outside named
   pipe access all fail.
3. Lifecycle controls: immediate fork, attempted breakaway, timeout, output
   flood, and forced supervisor termination leave no surviving child handles or
   delayed sentinel writes.
4. Repeat through public reviewed and clean-verification routes. Retain every
   failure, token/job diagnostic, executable digest, and native runner identity.
   Attach this evidence to the candidate-specific promotion record.

## Sources

- [Microsoft: implementing AppContainer and LPAC](https://learn.microsoft.com/en-us/windows/win32/secauthz/implementing-an-appcontainer)
- [Microsoft: process creation attributes](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute)
- [Microsoft: Job Objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects)
- [GitHub: hosted runner availability](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
- [GitHub: Windows ARM64 image inventory](https://github.com/actions/runner-images/blob/main/images/windows/Windows11-Arm64-Readme.md)

## Executing a candidate

The repository currently registers only CI and Release workflows on its default
branch. A newly added workflow cannot be manually dispatched until GitHub knows
it on the default branch. The probe workflow therefore also runs on a push to
`codex/plan-a-repair` that changes its code/workflow or Go dependency files.
Commit and push the reviewed candidate to that branch, then inspect the
`Windows sandbox feasibility probe` run and both architecture artifacts. This
probe does not publish or change production configuration. Its checkout token
is read-only and not persisted; the child environment contains no real secrets.

Record the workflow run URL and exact candidate SHA before interpreting results.
A skipped, missing, failed, or architecture-mismatched job is not native evidence.
Do not replace the Windows source snapshot implementation with POSIX `dir_fd` or
`O_NOFOLLOW` operations: production Windows materialization still needs native
handle/reparse-point validation.

## First native observation

Candidate `d766ab3`, run [34439690985](https://github.com/semanta-dev/codex-dispatch/actions/runs/34439690985),
executed on both Windows amd64 and arm64. AppContainer process creation
succeeded, but token inspection failed before primary-thread resume while
querying the LPAC field through the generic null-buffer sizing pattern. The
probe correctly stayed NO-GO; no child controls ran and cleanup reported no
errors. Fixed-size token fields now use direct DWORD queries and errors retain
the information class and Win32 error. Where the LPAC enum query is unsupported,
the probe checks the kernel-owned `WIN://NOALLAPPPKG` security attribute by exact
name through `NtQuerySecurityAttributesToken`. This follows the established
[System Informer implementation](https://github.com/winsiderss/systeminformer/blob/master/phlib/nativetoken.c),
which itself marks the direct LPAC enum query as TODO. A missing attribute or
unsupported query fails closed; requested creation flags are never evidence. This correction needs a fresh native run;
it is not evidence that LPAC or Git Bash passed.
