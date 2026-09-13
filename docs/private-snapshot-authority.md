# Private snapshot authority

New captures use a random run directory below the controller user's
`~/.codex-dispatch-authority`, resolving home through the account database rather
than caller-supplied `HOME`. Static Linux builds require a matching local
`/etc/passwd` entry and reject missing accounts; native lookup uses the effective
UID. POSIX roots must already be owned by that user
with no group/other permissions; existing unsafe roots are rejected rather than
chmodded. Windows root creation uses a protected current-user DACL with file
and directory inheritance. Windows behavior still requires native qualification.

Each run is a standalone bare Git repository containing raw captured blobs and
trees. Snapshot hashing has no object alternates and does not add WIP objects or
baseline refs to the source repository. The original index is retained as
`baseline-index` in this private directory; temporary indexes and raw hashing
inputs also remain there. Regular snapshot input is opened through a pinned
repository handle, bounded to 8 MiB per file, 512 MiB total and 100,000 files.

`authority.json` records the run ID, source HEAD/tree, original-index digest and
presence, a digest of the private indexed-object closure (including staged-only
blobs and sparse-index trees), shared-index bytes/digest for split indexes,
dispatch executable digest, repository, effective workdir and exact
export directory. It is created once at baseline capture. `capture.json` repeats
those bindings and seals the actual task patch digest at diff capture.
`terminal.json` binds the controller-produced `result.json` bytes and records
the broker session ID and outcome. Records are synced and published without
replacing an existing identity. Diff capture is not a terminal success verdict.

Dispatch keeps a baseline handle across the model turn. A changed public
snapshot descriptor is rejected; it cannot redirect object writes. Clean
verification resolves the opaque run ID only within the fixed controller root,
checks baseline/capture agreement and export identity, and materializes from the
private object store. It compares the public patch to the sealed private patch
and applies the exact checked bytes. Caller cwd cannot replace the bound
module workdir. The public result format remains unchanged.

## Retention and recovery

No automatic cleanup, Git pruning or history rewriting is performed. Failed or
interrupted captures can leave private directories, including completed
baseline recovery data. A run lacking the required immutable record is
incomplete and cannot qualify verification. Retain the entire private directory
alongside its export until recovery is no longer needed. A run cannot be resumed
by replacing or extending its immutable capture/terminal record; a fresh
attempt needs a new identity. Controller restart and broker-wide invocation
recovery remain an integration gate.

For inspection, use Git against the private bare repository, for example
`git --git-dir "$private_run" show "$tree:path/to/file"`. Recover into a separate
owner-only directory first. `baseline-index`, `index-objects.json`, and any referenced `sharedindex.<oid>` preserve staged-only and split-index staging independently of the source index;
`index_present: false` means the repository originally had no index. Do not copy
that empty placeholder into the user's Git index. Staged-only blobs already in
the user's Git database are not erased or retroactively made private.

Legacy v2 exports using repository baseline refs are unqualified for the new
private contract. Existing objects and index exports may already have been
exposed. Keep that historical evidence unchanged; inventory legacy
`refs/codex-dispatch/baselines/` and `baseline-index` exports before an explicit
migration. There is no automatic migration or deletion command in this change.
Deleting a private directory, legacy ref or loose object is not physical erasure.

Index closure capture bounds each Git output and shares a 120-second context
across inventory, metadata reads, object copying and recursive tree traversal.
Overflow or deadline expiration kills and reaps the direct Git process. Sparse
recovery is tested through task capture and Linux clean verification, then by
recovering the original index entries and excluded file bytes after removing
the source object directory. This closure budget does not yet bound every Git
operation elsewhere in baseline capture.

## Remaining qualification gates

This store is an access-controlled location, not a separate operating-system
principal. It does not protect against a dispatched same-user process with
unrestricted host access. Actual Codex sandbox exclusion, including resumed
sessions and custom policies, must be proven before reviewed-route promotion.
The installed Codex 0.154.0 local sandbox probe demonstrated that its default
workspace profile can read authority and that a custom TMPDIR inside authority
also permits writes. An authority-root deny alone does not close that narrower
grant. Default account-store creation now rejects temporary roots overlapping
authority in either direction, including symlink aliases, before creating a run
or writing recovery data. This guard does not enforce the missing read deny or
validate every configurable child writable root.
See [the retained probe](authority-sandbox-probe-2026-09-13.md) for controls
and required integration. Linux verification's separate mount namespace denies the tested private index
read/write attempts. Second-account access tests and both Windows DACL/native
runs are still required; cross-compilation is not native evidence.

Terminal records are produced for normal dispatch outcomes and cancellation.
Early setup/transport failures, abrupt controller death, protected reviewer
receipts/history, collector/acceptance readers and invocation identity through
all public wrappers remain unfinished. In particular, publishing `terminal.json`
does not yet make every existing workspace-evidence consumer authoritative.
Promotion remains NO-GO until those consumers, native backends and frozen-candidate
evidence are qualified independently.
