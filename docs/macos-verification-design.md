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
