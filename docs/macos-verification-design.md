# macOS verification feasibility

This is an experiment, not an implemented production backend. Reviewed
verification remains unsupported on macOS until native boundary and lifecycle
evidence establishes a usable policy.

The native probe executes only fixed Go and shell controls under a deny-default
Seatbelt profile. Runtime reads are explicit, writes are restricted to a
disposable directory, host file contents and networking are denied, and the
environment is explicitly constructed. A host connection proves the loopback
listener is reachable before the sandbox attempts the same address. The report
retains the full profile, positive/negative results, native architecture and
binary/candidate identity.

The second control starts a child in a separate session and kills the original
process group. Any delayed write is a failed lifecycle gate. The child is a
fixed bounded probe that exits itself, not an unbounded daemon. This experiment
tests the existing process-group cleanup assumption; it does not implement a
reliable owner or claim that Seatbelt supplies process ownership.

Native runs on Intel and Apple Silicon are required. A boundary pass alone
cannot authorize integration. Remaining work includes reliable descendant
ownership after controller failure, repeated interruption and deadlines;
resource limits; dependency/toolchain inputs; and legitimate Go/Python/Node
verification compatibility. No host fallback or reduction of supported-platform
qualification is implied by this experiment.

## Native startup investigation

Runs `34440616692` and `34440949208` failed on both architectures before
application code. Even `/usr/bin/true` and `/bin/sh` aborted. Retained crash
reports show `ignition_halt`, `boot_boot`, and `dyld4::CacheFinder`, termination
namespace `0x23`, code 2. These runs establish no child boundary or lifecycle
pass.

A [matching external diagnosis](https://github.com/lanefoundry/looplane/blob/HEAD/.research/macos-sandbox-diagnosis.md)
identifies dyld's root-directory open as the denied operation. The next native
experiment adds only `(allow file-read-data (literal "/"))`. This deliberately
permits enumeration of immediate root entries, not recursive file reads. All
fake-secret, outside-write and reachable-host network controls remain required.
The external report is a hypothesis for our environment until the native result
confirms startup; it is not substituted for our own qualification evidence.
