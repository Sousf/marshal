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
  commit as the change; skills and the operator profile land in `skills/`
  of the memory repo only as a pull request in the `memory` class, which
  never promotes. `skills.Check` refuses forbidden content before any PR
  exists and the verifier repeats the check on the diff. Run history fed to
  briefs is data the harness already holds, labelled as not instructions.
- A task without a repository gets no tokens and no repository; it writes
  only to its scratch directory.
- Pre-flight runs before every commit, derived from workflows when there
  is no contract, and every gate verb must pass before a PR is opened.
  Regressions always block; `--allow-red` only excuses pre-existing
  failures.
- With a contract, the gate is deterministic: a verb that passed at
  baseline and fails after the agent blocks the commit, in Go. Contract
  verbs run with credentials scrubbed. `marshal.yaml` in this repo is the
  reference example.
- Every autonomous merge is watched until the default branch's CI reports
  on it; red reverts and demotes. Human merges are never reverted by
  MARSHAL.
- The fleet's `never:` block only adds restrictions. Its tools are applied
  in `runner.Claude` itself and in chat, so no caller can skip them; its
  paths join every policy check; its `auto_merge` patterns are checked in
  `mayMerge` before anything else.
- Every agent invocation goes through `runner.Guard`. A quota stop holds
  the whole fleet until the reset, and a quota error never records a
  verifier verdict or opens an incident.
- Memory in a brief is capped (`skills.OperatorCap`, `skills.SkillCap`,
  `run.HistoryCap`) and comes before the per-run sections.
- The skill nudge has no tools and its answer goes through the same skill
  path as any proposal: `skills.Check`, then a memory-class PR.
- The `release` job is Go, not an agent (`run/release.go`). It only
  force-pushes `marshal/release`, which it rebuilds from the base every
  time. `VERSION` and `CHANGELOG.md` are always locked for agents, and
  release PRs get no follow-ups.
- See `SECURITY.md`: in-process checks are heuristics; only OS isolation
  is a boundary. Do not describe an allowlist as one.
- No autonomous merge without a PASS verdict bound to the PR's current head.
  The verifier gets read-only tools and never the proposer's transcript.
  `verifier.Checks.Fatal` outranks the model's answer.

Tests: `go test ./...` must pass with no network and no `claude` binary.
Anything that talks to an agent goes through `runner.Adapter` so tests can
substitute a fake. Stream parsing is tested against recorded output.

Comments explain why, not what. A comment that restates the next line is
deleted.
