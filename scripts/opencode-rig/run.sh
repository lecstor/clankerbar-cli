#!/usr/bin/env bash
# Run ONE opencode session the way the fleet does, with every outside
# dependency replaced. See README.md in this directory for the scenarios, the
# traps this wrapper guards against, and the cost.
#
#   scripts/opencode-rig/run.sh --model commandcode/deepseek/deepseek-v4.1-flash \
#       --provider-key-file ~/.config/clankerbar/commandcode-key --scenario padding
#
# The Go program does the real work (free port, readiness, deadline, call cap,
# isolated XDG dirs, proxy and stub). This wrapper resolves the repo root and
# runs the package there, so it works from any directory and any module.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)

# `go -C` runs the module in $root, so the package resolves against THIS repo's
# go.mod no matter which module the caller's directory belongs to. Without it,
# `go run <abs dir>` resolves the package against the caller's module and fails
# with "directory ... outside main module".
exec go -C "$root" run ./scripts/opencode-rig "$@"
