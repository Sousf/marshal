# MARSHAL conventions

Go, standard library first. Dependencies are added only when the standard
library would cost more than a page of code, and never for a single helper.

Layout: `main.go` is the CLI. Everything else is under `internal/`, one
package per concern: `config` (fleet file), `runner` (agent adapters),
`store` (SQLite), `workspace` (git and PRs), `policy` (diff checks and the
trust ladder), `run` (one job end to end), `jobs` (embedded briefs),
`sched` (schedule strings), `github` (gh wrapper), `api` (Unix socket
protocol), `daemon` (scheduler, PR sync, auto-merge), `verifier` (the
second agent and its deterministic checks), `ui` (Bubble Tea).

Rules the code enforces, which no change may weaken:

- The agent never commits, pushes, or opens PRs. `workspace` does, after
  `policy.Check` has passed.
- Caps and allowlists come from the fleet file, read by Go. Nothing in a
  prompt can raise them.
- `policy.alwaysLocked` paths cannot be unlocked by any job configuration.
- The starter fleet in `starter.go` and `examples/fleet.yaml` stay identical;
  a test checks.
- Only a human merge counts as an approval on the ladder. Machine merges
  never promote.
- Auto-merge requires CI checks to exist and pass. A repo without CI gets no
  autonomy, whatever its level.
- The daemon listens on a Unix socket only.
- Memory goes through review: lessons land in AGENTS.md inside the same
  commit as the change, never anywhere else, and `run.ParseLessons` only
  accepts plain bullets. Run history fed to briefs is data the harness
  already holds, labelled as not instructions.
- Every autonomous merge is watched until the default branch's CI reports
  on it; red reverts and demotes. Human merges are never reverted by
  MARSHAL.
- No autonomous merge without a PASS verdict bound to the PR's current head.
  The verifier gets read-only tools and never the proposer's transcript.
  `verifier.Checks.Fatal` outranks the model's answer.

Tests: `go test ./...` must pass with no network and no `claude` binary.
Anything that talks to an agent goes through `runner.Adapter` so tests can
substitute a fake. Stream parsing is tested against recorded output.

Comments explain why, not what. A comment that restates the next line is
deleted.
