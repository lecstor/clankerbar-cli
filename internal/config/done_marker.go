package config

// DoneMarker opens a runner-claimed session's done report: the line after which
// its final message carries one JSON object the runner turns into the task's
// records (see docs/runner-owned-mechanics.md, B8). It must not collide with
// HandoffMarker, and the parser and the briefs must name the same string.
const DoneMarker = "=====CLANKERBAR DONE====="
