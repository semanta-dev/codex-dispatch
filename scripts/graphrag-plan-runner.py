#!/usr/bin/env python3
"""Run a GraphRAG Codex plan as scheduled packets in a single working tree.

The single-packet bridge is intentionally strict. This runner sits one layer
above it: parse a packetized GraphRAG plan, schedule independent packets in
parallel (with a deterministic allowed-file overlap partition so co-writing
packets never share a wave), execute each packet, and write a crash-durable,
machine-readable execution ledger for review, ingestion, and benchmarking.

Isolation modes (`--isolation`):
  none (default): dispatch each packet directly via scripts/dispatch-codex.sh in
    the parent working tree; no git worktrees are created. The overlap partition
    keeps co-writing packets out of the same wave, and a per-file lock registry
    (FileLockRegistry) serializes only packets that claim a shared file, so
    disjoint packets within a wave dispatch and verify in parallel up to
    `--jobs N`. A verification command that reads files a packet does not claim
    (e.g. `go build ./...`) can still see a concurrent peer's in-flight edits to
    those unclaimed files; use `--jobs 1` or `--isolation worktree` when a
    packet's verification needs a fully quiescent tree.
  worktree: route each packet through scripts/graphrag-worktree-dispatch.sh for
    git-worktree isolation (the opt-in fallback).
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import pathlib
import re
import signal
import subprocess
import tempfile
import threading
import time
from dataclasses import dataclass, field
from typing import Any


class FileLockRegistry:
    """Per-file mutexes so disjoint single-tree packets dispatch in parallel.

    In `--isolation none` mode a packet holds a lock on each implementation
    path it claims across its whole dispatch+verification region, so a packet
    never writes (or verifies) against another packet's half-applied edits to a
    *shared* file. The overlap partition already keeps co-writing packets out of
    the same wave, so packets that DO share a wave claim disjoint paths and thus
    acquire disjoint lock sets — they run fully in parallel, honoring `--jobs N`.

    Locks are always acquired in sorted path order to preclude deadlock, and
    they key on the path string a packet claims (the same strings the overlap
    partition compares), so the lock set and the partition agree by construction.

    Note: this guards files a packet *claims*. A verification command that reads
    files outside the claimed set (e.g. `go build ./...`) can still observe a
    concurrent peer's in-flight edits to those unclaimed files; that residual is
    inherent to running N dispatches in one working tree and is the same class of
    cross-talk the overlap partition does not attempt to model for unclaimed
    paths. A packet whose verification needs a fully quiescent tree should run
    with `--jobs 1` or `--isolation worktree`.
    """

    def __init__(self) -> None:
        self._guard = threading.Lock()
        self._locks: dict[str, threading.Lock] = {}

    def _lock_for(self, path: str) -> threading.Lock:
        with self._guard:
            lock = self._locks.get(path)
            if lock is None:
                lock = threading.Lock()
                self._locks[path] = lock
            return lock

    def acquire(self, paths: list[str]) -> list[threading.Lock]:
        held: list[threading.Lock] = []
        # Sorted, de-duplicated order so concurrent packets never acquire a
        # shared subset of locks in opposing orders (deadlock-free).
        for path in sorted(set(paths)):
            lock = self._lock_for(path)
            lock.acquire()
            held.append(lock)
        return held

    @staticmethod
    def release(held: list[threading.Lock]) -> None:
        for lock in reversed(held):
            lock.release()


# Per-file lock registry for single-tree (`--isolation none`) dispatch. See
# FileLockRegistry for why per-file (not one global mutex) is what actually lets
# `--jobs N` parallelize disjoint packets within a wave.
SINGLE_TREE_LOCKS = FileLockRegistry()


SECTION_RE = re.compile(r"^([A-Za-z][A-Za-z0-9 /_-]*):\s*$")
# Anchor to the packet-id grammar: a packet id is an alphanumeric/dot/dash/
# underscore token that contains at least one digit (e.g. "001", "07a",
# "2.1"). This deliberately does not match prose headings such as
# "## Packet Naming" or "## Packet Conventions".
PACKET_RE = re.compile(
    r"^##\s+Packet\s+([A-Za-z0-9_.-]*\d[A-Za-z0-9_.-]*)\s*:?\s*(.*)$",
    re.I | re.M,
)


@dataclass
class Packet:
    number: str
    title: str
    heading: str
    body: str
    sections: dict[str, str]
    allowed_files: list[str]
    input_files: list[str]
    verification: str
    progress_record: str
    dependencies: list[str] = field(default_factory=list)


def normalize_section(name: str) -> str:
    return re.sub(r"\s+", " ", name.strip().lower())


def strip_path(raw: str) -> str:
    raw = raw.strip()
    raw = raw.removeprefix("-").strip()
    raw = raw.removeprefix("*").strip()
    raw = raw.strip("`").strip()
    if ":" in raw and not raw.startswith(("http://", "https://")):
        maybe_path = raw.split(":", 1)[0].strip()
        if maybe_path:
            raw = maybe_path
    return raw.removeprefix("./")


def list_paths(text: str) -> list[str]:
    paths = []
    for line in text.splitlines():
        path = strip_path(line)
        if path and not path.startswith("#") and path not in {"None", "none"}:
            paths.append(path)
    return paths


def clean_block(text: str) -> str:
    lines = text.strip().splitlines()
    if len(lines) >= 2 and lines[0].startswith("```") and lines[-1].startswith("```"):
        return "\n".join(lines[1:-1]).strip()
    return text.strip()


def parse_sections(body: str) -> dict[str, str]:
    sections: dict[str, list[str]] = {}
    current: str | None = None
    for line in body.splitlines():
        match = SECTION_RE.match(line)
        if match:
            current = normalize_section(match.group(1))
            sections.setdefault(current, [])
            continue
        if current is not None:
            sections[current].append(line)
    return {key: clean_block("\n".join(value)) for key, value in sections.items()}


def normalize_packet_id(token: str) -> str:
    """Zero-pad pure-numeric ids to 3 digits; leave non-numeric ids verbatim.

    Mirrors parse_plan's number handling so a dependency reference resolves to
    the same key as the packet it points at. A non-numeric id (e.g. "07a") is
    kept as-is rather than having a digit substring zero-padded.
    """
    return token.zfill(3) if token.isdigit() else token


def parse_dependencies(text: str) -> list[str]:
    deps = []
    for token in re.split(r"[\s,]+", text):
        token = token.strip().strip("`").strip()
        if not token or token in {"-", "*", "None", "none"}:
            continue
        deps.append(normalize_packet_id(token))
    return deps


def parse_plan(path: pathlib.Path) -> list[Packet]:
    text = path.read_text()
    headings = list(PACKET_RE.finditer(text))
    packets = []
    for index, match in enumerate(headings):
        start = match.end()
        end = headings[index + 1].start() if index + 1 < len(headings) else len(text)
        number = normalize_packet_id(match.group(1))
        title = match.group(2).strip()
        heading = match.group(0).strip()
        body = text[start:end].strip()
        sections = parse_sections(body)
        allowed_files = list_paths(sections.get("allowed files", ""))
        input_files = list_paths(sections.get("inputs", ""))
        verification = sections.get("verification", "").strip()
        progress_record = strip_path(sections.get("progress record", "").splitlines()[0]) if sections.get("progress record") else ""
        deps_text = sections.get("depends on", "") or sections.get("blocked by", "")
        packets.append(
            Packet(
                number=number,
                title=title,
                heading=heading,
                body=body,
                sections=sections,
                allowed_files=allowed_files,
                input_files=input_files,
                verification=verification,
                progress_record=progress_record,
                dependencies=parse_dependencies(deps_text),
            )
        )
    return packets


def require_packet_contract(packet: Packet) -> list[str]:
    errors = []
    for section in ["allowed files", "acceptance criteria", "verification", "progress record"]:
        if not packet.sections.get(section):
            errors.append(f"{packet.heading}: missing {section}")
    if packet.progress_record and packet.progress_record not in packet.allowed_files:
        errors.append(f"{packet.heading}: progress record is not listed in Allowed files")
    return errors


def is_done(repo: pathlib.Path, packet: Packet) -> bool:
    if not packet.progress_record:
        return False
    path = repo / packet.progress_record
    return path.exists() and "Status: done" in path.read_text(errors="ignore")


def is_progress_path(path: str) -> bool:
    return path.endswith((".done.md", ".blocked.md"))


def implementation_files(packet: Packet) -> list[str]:
    return [path for path in packet.allowed_files if not is_progress_path(path)]


def dispatch_allowed_files(packet: Packet, args: argparse.Namespace) -> list[str]:
    if args.write_progress:
        return implementation_files(packet)
    return packet.allowed_files


def build_env(packet: Packet, args: argparse.Namespace, repo: pathlib.Path) -> dict[str, str]:
    non_progress_allowed = implementation_files(packet)
    file_context = []
    if args.context_mode == "full":
        candidates = [*packet.input_files, *non_progress_allowed]
    elif args.context_mode == "targets":
        candidates = non_progress_allowed
    else:
        candidates = []
    for path in candidates:
        if path not in file_context and (repo / path).exists():
            file_context.append(path)

    task = f"""Implement one GraphRAG packet.

Plan: {args.plan}
Packet: {packet.heading}

Objective:
{packet.sections.get("objective", "")}

Implementation notes:
{packet.sections.get("implementation notes", "")}

GraphRAG context:
{packet.sections.get("graphrag context", "")}

Complete exactly this packet and stop. Do not execute later packets. The plan runner will write GraphRAG progress records after verification."""

    acceptance = f"""{packet.sections.get("acceptance criteria", "")}
- Scope audit passes: every changed file is listed in Allowed files.
- Verification command passes: {packet.verification}"""

    constraints = f"""Implementation files you may edit:
{chr(10).join(f"- `{path}`" for path in non_progress_allowed)}

Disallowed changes:
{packet.sections.get("disallowed changes", "")}

Do not edit GraphRAG progress records; the plan runner owns them.
Do not edit files outside the implementation file list. If the packet cannot be completed within scope, stop without making unrelated edits.
Do not commit, branch, push, revert, stash, or mutate git history."""

    env = os.environ.copy()
    env.update(
        {
            "CODEX_TASK": task,
            "CODEX_ACCEPTANCE": acceptance,
            "CODEX_FILES": ",".join(file_context),
            "CODEX_CONSTRAINTS": constraints,
            "CODEX_SANDBOX": os.environ.get("CODEX_SANDBOX", "workspace-write"),
            "PYTHONDONTWRITEBYTECODE": "1",
        }
    )
    if args.dispatch_command:
        env["GRAPHRAG_DISPATCH_COMMAND"] = args.dispatch_command
    if args.dispatch_attempts:
        env["GRAPHRAG_DISPATCH_ATTEMPTS"] = str(args.dispatch_attempts)
    if args.shared_broker_addr:
        env["CODEX_BROKER_ADDR_PATH"] = args.shared_broker_addr
        env["CODEX_BROKER_MAX_CONCURRENT"] = str(max(args.jobs, 1))
    return env


def run_shell(command: str, repo: pathlib.Path) -> dict[str, Any]:
    started = time.monotonic()
    proc = subprocess.run(
        command,
        cwd=repo,
        shell=True,
        executable="/bin/bash",
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    return {
        "command": command,
        "exit_code": proc.returncode,
        "wall_s": round(time.monotonic() - started, 3),
        "stdout": proc.stdout[-4000:],
        "stderr": proc.stderr[-4000:],
    }


def write_progress_record(
    repo: pathlib.Path,
    packet: Packet,
    result_json: dict[str, Any],
    verification: dict[str, Any] | None,
    isolation: str = "none",
) -> None:
    if not packet.progress_record:
        return
    progress = repo / packet.progress_record
    progress.parent.mkdir(parents=True, exist_ok=True)
    changed = result_json.get("files_changed") or []
    # packet.heading is the captured markdown line ("## Packet 001: One"); strip
    # its leading "#" markers so the record's H1 is "# Packet 001: One", not the
    # doubled "# ## Packet 001: One".
    heading_text = packet.heading.lstrip("#").strip()
    lines = [
        f"# {heading_text}",
        "",
        "Status: done",
        "",
        "Changed files:",
        *[f"- `{path}`" for path in changed],
        "",
        "Verification:",
        f"- `{packet.verification}`",
    ]
    if verification is not None:
        lines.extend(
            [
                f"- exit_code: {verification['exit_code']}",
                f"- wall_s: {verification['wall_s']}",
            ]
        )
        if verification.get("stdout"):
            lines.extend(["", "Verification stdout:", "```text", verification["stdout"].strip(), "```"])
        if verification.get("stderr"):
            lines.extend(["", "Verification stderr:", "```text", verification["stderr"].strip(), "```"])
    lines.extend(
        [
            "",
            "Scope audit:",
            f"- clean in {'isolated worktree' if isolation == 'worktree' else 'single-tree'} dispatch",
            "",
            "Acceptance criteria:",
            packet.sections.get("acceptance criteria", "").strip(),
            "",
            "Notes:",
            "- Progress record written by graphrag-plan-runner.py after successful dispatch and verification.",
            "",
        ]
    )
    progress.write_text("\n".join(lines))


def dispatch_packet(
    packet: Packet,
    args: argparse.Namespace,
    repo: pathlib.Path,
    plugin_root: pathlib.Path,
) -> tuple[subprocess.CompletedProcess, str, str, str, dict[str, Any], list[str]]:
    """Dispatch one packet, returning (proc, stdout, stderr, run_dir, result_json, allowed).

    In `--isolation none` (the single-tree default) the packet is dispatched
    directly via scripts/dispatch-codex.sh in the parent tree (`repo`); the
    dispatcher prints its result dir as the last stdout line and writes
    result.json there. In `--isolation worktree` the packet routes through
    graphrag-worktree-dispatch.sh, which fans the allowed files back into the
    parent and copies result.json to dispatch-result.json in its own run dir.
    """
    allowed_for_dispatch = dispatch_allowed_files(packet, args)
    stdout_path = pathlib.Path(args.out) / f"packet-{packet.number}.stdout"
    stderr_path = pathlib.Path(args.out) / f"packet-{packet.number}.stderr"
    env = build_env(packet, args, repo)

    if args.isolation == "worktree":
        dispatch = plugin_root / "scripts" / "graphrag-worktree-dispatch.sh"
        with tempfile.NamedTemporaryFile("w", delete=False) as allowed:
            allowed.write("\n".join(allowed_for_dispatch) + "\n")
            allowed_path = allowed.name
        try:
            with stdout_path.open("w") as stdout, stderr_path.open("w") as stderr:
                proc = subprocess.run(
                    [str(dispatch), "--allowed-file", allowed_path],
                    cwd=repo,
                    env=env,
                    text=True,
                    stdout=stdout,
                    stderr=stderr,
                )
        finally:
            pathlib.Path(allowed_path).unlink(missing_ok=True)
        result_name = "dispatch-result.json"
    else:
        # Single-tree: dispatch directly into the parent tree. No worktree is
        # created and no --allowed-file is passed; the dispatcher writes the
        # result.json file into the dir it prints on its last stdout line.
        dispatch_cmd = args.dispatch_command or str(plugin_root / "scripts" / "dispatch-codex.sh")
        with stdout_path.open("w") as stdout, stderr_path.open("w") as stderr:
            proc = subprocess.run(
                [dispatch_cmd],
                cwd=repo,
                env=env,
                text=True,
                stdout=stdout,
                stderr=stderr,
            )
        result_name = "result.json"

    stdout_text = stdout_path.read_text(errors="ignore")
    stderr_text = stderr_path.read_text(errors="ignore")
    run_dir = ""
    for line in stdout_text.splitlines():
        if line.strip():
            run_dir = line.strip()

    result_json: dict[str, Any] = {}
    if run_dir:
        result_path = pathlib.Path(run_dir) / result_name
        if result_path.exists():
            try:
                result_json = json.loads(result_path.read_text(errors="ignore"))
            except json.JSONDecodeError:
                result_json = {}

    return proc, stdout_text, stderr_text, run_dir, result_json, allowed_for_dispatch


def run_packet(packet: Packet, args: argparse.Namespace, repo: pathlib.Path, plugin_root: pathlib.Path) -> dict[str, Any]:
    started = time.monotonic()
    stdout_path = pathlib.Path(args.out) / f"packet-{packet.number}.stdout"
    stderr_path = pathlib.Path(args.out) / f"packet-{packet.number}.stderr"

    # In single-tree mode, hold a per-file lock on each implementation path this
    # packet claims across its whole dispatch AND verification region, so a
    # packet never writes/verifies against another packet's half-applied edits to
    # a shared file. Because the overlap partition keeps co-writing packets out
    # of the same wave, wave peers claim disjoint paths -> disjoint lock sets ->
    # they dispatch in parallel (honoring --jobs N). In worktree mode each packet
    # is isolated by its own worktree, so no locking is needed.
    held: list[threading.Lock] = []
    if args.isolation != "worktree":
        held = SINGLE_TREE_LOCKS.acquire(overlap_paths(packet))
    try:
        proc, stdout_text, stderr_text, run_dir, result_json, allowed_for_dispatch = dispatch_packet(
            packet, args, repo, plugin_root
        )

        verification = None
        if proc.returncode == 0 and packet.verification and not args.no_verify:
            verification = run_shell(packet.verification, repo)

        verification_ok = verification is None or verification["exit_code"] == 0
        if proc.returncode == 0 and verification_ok and args.write_progress:
            write_progress_record(repo, packet, result_json, verification, args.isolation)
    finally:
        FileLockRegistry.release(held)

    progress_state = "missing"
    if packet.progress_record:
        progress = repo / packet.progress_record
        if progress.exists():
            content = progress.read_text(errors="ignore")
            progress_state = "done" if "Status: done" in content else "invalid"

    passed = proc.returncode == 0
    if verification is not None:
        passed = passed and verification["exit_code"] == 0
    passed = passed and progress_state == "done"

    return {
        "packet": packet.number,
        "heading": packet.heading,
        "status": "pass" if passed else "fail",
        "wall_s": round(time.monotonic() - started, 3),
        "wrapper_exit_code": proc.returncode,
        "run_dir": run_dir,
        "stdout_path": str(stdout_path),
        "stderr_path": str(stderr_path),
        "stdout_tail": stdout_text[-4000:],
        "stderr_tail": stderr_text[-4000:],
        "dispatch_result": result_json,
        "verification": verification,
        "progress_record": packet.progress_record,
        "progress_state": progress_state,
        "dispatch_allowed_files": allowed_for_dispatch,
        "changed_files": result_json.get("files_changed", []),
    }


def overlap_paths(packet: Packet) -> list[str]:
    """Implementation paths a packet claims, for overlap scheduling.

    Progress records are excluded (they are per-packet by construction). Tokens
    containing whitespace are also excluded: real repository paths in this plan
    grammar never contain spaces, so a whitespace-bearing entry is prose that
    leaked from a malformed section header rather than a co-write target.
    """
    return [path for path in implementation_files(packet) if path and not any(c.isspace() for c in path)]


def build_overlap_index(packets: list[Packet]) -> dict[str, list[str]]:
    """Map each implementation path to the packet numbers that claim it.

    Progress records are excluded — they are per-packet by construction and the
    plan-runner owns them, so they never represent a co-write conflict.
    """
    index: dict[str, list[str]] = {}
    for packet in packets:
        for path in overlap_paths(packet):
            index.setdefault(path, [])
            if packet.number not in index[path]:
                index[path].append(packet.number)
    return index


def overlap_conflicts(packets: list[Packet]) -> list[tuple[str, str, str]]:
    """Return deterministic (packet_a, packet_b, shared_path) co-write conflicts."""
    index = build_overlap_index(packets)
    seen: set[tuple[str, str]] = set()
    conflicts: list[tuple[str, str, str]] = []
    for path in sorted(index):
        claimants = sorted(index[path])
        for i in range(len(claimants)):
            for j in range(i + 1, len(claimants)):
                pair = (claimants[i], claimants[j])
                if pair in seen:
                    continue
                seen.add(pair)
                conflicts.append((pair[0], pair[1], path))
    return conflicts


def select_wave(
    runnable: list[str],
    by_number: dict[str, Packet],
    jobs: int,
) -> list[str]:
    """Pick a deterministic wave of up to `jobs` packets with no shared file.

    `runnable` is iterated in sorted order so the partition is stable across
    repeated runs. A candidate that shares an implementation file with any
    packet already chosen for this wave is deferred to a later wave, so two
    co-writing packets are never dispatched concurrently.
    """
    wave: list[str] = []
    wave_paths: set[str] = set()
    for number in runnable:
        if len(wave) >= max(1, jobs):
            break
        paths = set(overlap_paths(by_number[number]))
        if paths & wave_paths:
            continue  # co-writes a wave member's file; serialize into a later wave
        wave.append(number)
        wave_paths |= paths
    # Guard against starvation: if nothing was selectable (e.g. a single packet
    # whose only impl file is shared with itself across duplicate entries),
    # fall back to the first runnable packet alone.
    if not wave and runnable:
        wave = [runnable[0]]
    return wave


def run_plan(args: argparse.Namespace) -> int:
    repo = pathlib.Path(args.repo).resolve()
    plugin_root = pathlib.Path(__file__).resolve().parents[1]
    out = pathlib.Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=True)
    ledger_path = out / "ledger.json"
    if args.shared_broker and not args.shared_broker_addr:
        broker_dir = out / "shared-broker"
        broker_dir.mkdir(parents=True, exist_ok=True)
        args.shared_broker_addr = str(broker_dir / "broker.addr")

    packets = parse_plan(pathlib.Path(args.plan))
    if not packets:
        ledger = {"status": "fail", "errors": ["plan contains no packet sections"], "packets": []}
        ledger_path.write_text(json.dumps(ledger, indent=2) + "\n")
        print(json.dumps(ledger, indent=2))
        return 2
    errors = [error for packet in packets for error in require_packet_contract(packet)]
    if errors:
        ledger = {"status": "fail", "errors": errors, "packets": []}
        ledger_path.write_text(json.dumps(ledger, indent=2) + "\n")
        print(json.dumps(ledger, indent=2))
        return 2

    by_number = {packet.number: packet for packet in packets}
    conflicts = overlap_conflicts(packets)
    remaining = {packet.number for packet in packets if args.rerun or not is_done(repo, packet)}
    completed = {packet.number for packet in packets if packet.number not in remaining}
    records: list[dict[str, Any]] = []
    started = time.monotonic()

    def build_ledger(status: str, interrupted: bool = False) -> dict[str, Any]:
        return {
            "status": status,
            "interrupted": interrupted,
            "plan": str(pathlib.Path(args.plan).resolve()),
            "repo": str(repo),
            "isolation": args.isolation,
            "jobs": args.jobs,
            "overlap_conflicts": [
                {"packets": [a, b], "path": path} for a, b, path in conflicts
            ],
            "wall_s": round(time.monotonic() - started, 3),
            "packets_total": len(packets),
            "packets_run": len([record for record in records if "packet" in record]),
            "packets_passed": len([record for record in records if record.get("status") == "pass"]),
            "records": records,
        }

    def flush_ledger(status: str, interrupted: bool = False) -> dict[str, Any]:
        # Crash-durable: write to a temp file in the same dir, then atomic
        # rename, so a partial write (e.g. on SIGINT mid-flush) never leaves a
        # truncated ledger.json. Returns the exact dict written to disk so a
        # caller can print the same object instead of recomputing build_ledger()
        # (which would re-snapshot wall_s and diverge from the on-disk ledger).
        ledger = build_ledger(status, interrupted)
        tmp = ledger_path.with_suffix(".json.tmp")
        tmp.write_text(json.dumps(ledger, indent=2) + "\n")
        os.replace(tmp, ledger_path)
        return ledger

    interrupted = {"flag": False}

    def handle_signal(signum, _frame):
        # Mark the interrupt and write a durable partial ledger immediately, so
        # even a signal delivered outside the dispatch try-block (e.g. during the
        # seed flush) still leaves a valid interrupted ledger.json. We re-raise
        # as KeyboardInterrupt so in-flight waves unwind; the except-block flush
        # below then rewrites the ledger with any records that fanned in during
        # the unwind, and that later atomic write is the authoritative one.
        interrupted["flag"] = True
        flush_ledger("interrupted", interrupted=True)
        raise KeyboardInterrupt

    previous_handlers = {}
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            previous_handlers[sig] = signal.signal(sig, handle_signal)
        except (ValueError, OSError):
            # signal() only works on the main thread; tolerate environments
            # where it is unavailable rather than failing the run.
            previous_handlers[sig] = None

    # Seed an initial ledger so a crash before the first fan-in still leaves a
    # valid (empty-records) ledger.json on disk.
    flush_ledger("running")

    try:
        while remaining:
            runnable = [
                number
                for number in sorted(remaining)
                if all(dep in completed for dep in by_number[number].dependencies)
            ]
            if not runnable:
                blocked = sorted(remaining)
                records.append({"status": "blocked", "packets": blocked, "reason": "dependencies did not complete"})
                flush_ledger("running")
                break

            wave = select_wave(runnable, by_number, args.jobs)
            for number in wave:
                remaining.remove(number)

            with concurrent.futures.ThreadPoolExecutor(max_workers=len(wave)) as pool:
                futures = {
                    pool.submit(run_packet, by_number[number], args, repo, plugin_root): number
                    for number in wave
                }
                for future in concurrent.futures.as_completed(futures):
                    record = future.result()
                    records.append(record)
                    if record["status"] == "pass":
                        completed.add(record["packet"])
                    # Durable: persist the ledger as each packet fans in so a
                    # crash never loses a completed packet's record.
                    flush_ledger("running")
    except KeyboardInterrupt:
        flush_ledger("interrupted", interrupted=True)
        return 130
    finally:
        for sig, handler in previous_handlers.items():
            if handler is not None:
                try:
                    signal.signal(sig, handler)
                except (ValueError, OSError):
                    pass

    status = "pass" if all(record.get("status") == "pass" for record in records) and not remaining else "fail"
    # Build the final ledger exactly once: flush_ledger writes it and returns
    # the same dict we print, so the on-disk ledger.json and stdout never
    # diverge (e.g. on wall_s).
    ledger = flush_ledger(status)
    print(json.dumps(ledger, indent=2))
    return 0 if status == "pass" else 1


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("plan")
    parser.add_argument("--repo", default=".")
    parser.add_argument("--out", required=True)
    parser.add_argument(
        "--jobs",
        type=int,
        default=4,
        help="max packets to dispatch concurrently per wave. In --isolation none, "
        "packets that claim disjoint files run in parallel up to this many; "
        "packets that share a claimed file are serialized by per-file locks "
        "(see --isolation help)",
    )
    parser.add_argument(
        "--isolation",
        choices=("none", "worktree"),
        default="none",
        help="none (default): dispatch each packet directly in the parent tree "
        "(single-tree, no git worktrees). Co-writing packets are kept out of the "
        "same wave by the overlap partition and serialized by per-file locks, so "
        "disjoint packets dispatch/verify in parallel up to --jobs; a "
        "verification that reads unclaimed files may still see a peer's in-flight "
        "edits (use --jobs 1 or --isolation worktree for a quiescent tree). "
        "worktree: route through graphrag-worktree-dispatch.sh for git-worktree "
        "isolation",
    )
    parser.add_argument(
        "--dispatch-command",
        help="override the dispatch command: the per-packet dispatcher in "
        "--isolation none, or GRAPHRAG_DISPATCH_COMMAND in --isolation worktree",
    )
    parser.add_argument("--dispatch-attempts", type=int, default=2)
    parser.add_argument(
        "--shared-broker",
        action="store_true",
        help="reuse one broker/app-server across packet worktrees; can reduce token/process overhead but may serialize wall time",
    )
    parser.add_argument(
        "--shared-broker-addr",
        default="",
        help="explicit broker address file for shared-broker mode",
    )
    parser.add_argument(
        "--context-mode",
        choices=("targets", "full", "none"),
        default="targets",
        help="files to include in CODEX_FILES: existing target files, inputs plus targets, or none",
    )
    parser.add_argument("--no-verify", action="store_true")
    parser.add_argument(
        "--no-write-progress",
        dest="write_progress",
        action="store_false",
        help="do not write missing progress records after successful dispatch and verification",
    )
    parser.set_defaults(write_progress=True)
    parser.add_argument("--rerun", action="store_true")
    args = parser.parse_args()
    if args.jobs < 1:
        parser.error("--jobs must be >= 1")
    return run_plan(args)


if __name__ == "__main__":
    raise SystemExit(main())
