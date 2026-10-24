# clankerbar-cli - agent guide

The local driver for clankerbar: it runs coding-agent sessions against a project's
backlog on clankerbar.com. Its backlog lives on clankerbar.com (project Clankerbar,
tasks with `repo: lecstor/clankerbar-cli`), not in this repo. Task PRs target `staging`;
only the operator merges the `staging -> main` promotion, which publishes a release
(`docs/releases.md`).

## Conventions

- **Small files, small functions.** Keep a source file under ~1,000 lines and to one
  concern, and a function under ~150. When a change would take a file past that, split it
  by concern first, as its own pure-move commit, then make the change. Test files mirror
  the source split. A reviewer raises an oversized file or function as a SHOULD. Why: an
  agent pays for every line it reads to find the part it needs (operator decision
  2026-10-22, CLA-794).
- **Comments state the why, briefly.** A comment says what someone editing this code must
  know not to break it: the invariant, the trap, the non-obvious constraint. Usually one
  to three lines. Keep out of comments: task history (which task changed what, what was
  measured when) - that belongs in the commit message and the task; and design, process
  or workflow reasoning - that belongs in `docs/`. A comment may point to either
  (`see docs/x.md`, `CLA-123`) instead of retelling it. Do not match the comment density
  of surrounding code when that density is narrative.

## Gates

`go build ./...`, `go vet ./...` and `go test ./...`. When the opencode adapter changes,
also run the hermetic conformance suite (`docs/harness-conformance.md`).
