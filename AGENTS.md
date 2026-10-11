# MARSHAL conventions

Go, standard library first. Dependencies are added only when the standard
library would cost more than a page of code, and never for a single helper.

Layout: `main.go` is the CLI. Everything else is under `internal/`, one
package per concern: `config` (fleet file), `runner` (agent adapters),
`store` (SQLite), `workspace` (git and PRs), `policy` (diff checks and the
trust ladder), `run` (one job end to end), `jobs` (embedded briefs).

Rules the code enforces, which no change may weaken:

- The agent never commits, pushes, or opens PRs. `workspace` does, after
  `policy.Check` has passed.
- Caps and allowlists come from the fleet file, read by Go. Nothing in a
  prompt can raise them.
- `policy.alwaysLocked` paths cannot be unlocked by any job configuration.
- The starter fleet in `starter.go` and `examples/fleet.yaml` stay identical;
  a test checks.

Tests: `go test ./...` must pass with no network and no `claude` binary.
Anything that talks to an agent goes through `runner.Adapter` so tests can
substitute a fake. Stream parsing is tested against recorded output.

Comments explain why, not what. A comment that restates the next line is
deleted.
