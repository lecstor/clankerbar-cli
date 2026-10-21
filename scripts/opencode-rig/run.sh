#!/usr/bin/env bash
# Run ONE opencode session the way the fleet does, with every outside
# dependency replaced. See README.md in this directory for the scenarios, the
# traps this wrapper guards against, and the cost.
#
#   scripts/opencode-rig/run.sh --model commandcode/deepseek/deepseek-v4.1-flash \
#       --provider-key-file ~/.config/clankerbar/commandcode-key --scenario padding
#
# The Go program does the real work (free port, readiness, deadline, call cap,
# isolated XDG dirs, proxy and stub). This wrapper only resolves the repo root
# so `go run` works from anywhere.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)

exec go run "$root/scripts/opencode-rig" "$@"
