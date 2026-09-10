#!/usr/bin/env bash
# Materialize the recorded Git objects without checkout hooks or filters, apply
# the dispatch patch, then run verification through execution_policy confinement.
# Usage: clean-verify.sh <run_dir> <verify-cmd> [args...]
# Exit: 2 usage, 6 missing/invalid baseline, 65 invalid patch/materialization,
# 66 source mutation, otherwise verifier status (126 unsupported sandbox).
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
exec python3 -I "$script_dir/clean_verify.py" "$@"
