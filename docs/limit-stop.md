# Stop-limit exits under the supervisor

A child whose harness reports a Stop-class usage limit (`Limit.Stop`, for example
`budget_exhausted`: credits gone, no stated reset) ends its run with
`iteration N stopped: <reason> - no reset to wait for, stopping (resume once resolved)`.
Before it exits, the daemon writes a `LIMIT_STOP` marker into its state dir; the first
line is the limit reason.

The supervisor reads that marker on the child's exit. It does not schedule a restart:
the instance stays stopped until the operator resumes it (decision 4468b833, CLA-835).
The fleet listing shows `stopped: <reason>` and the status report sends state `stopped`
with reason `stopped: <reason>`, never `restarting in ...`. An ordinary crash, with no
marker, still goes through the backoff ladder.

## Resume

Resolve the cause (top up credits, switch provider), then delete the marker:

    rm <state-dir>/LIMIT_STOP

The supervisor notices on its next poll, logs `LIMIT_STOP marker cleared - resuming` and
respawns the child. The marker also survives a supervisor restart, so the instance stays
stopped across one until the file is deleted. The state dir is `<roster cache dir>/<instance>-<hash>`,
printed in the `stopped on ...` log line.

An unsupervised `clankerbar run` clears a leftover marker at startup; starting it is the
resume.

`HALT` is a different switch (the operator stops an instance on purpose, and the
supervisor must be restarted after deleting it). `LIMIT_STOP` is written by the daemon
and cleared by deleting the file alone.
