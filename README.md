# MARSHAL

**M**aintains, **A**udits, **R**epairs, **S**ecures, **H**ardens, **A**nd **L**earns.

MARSHAL keeps a fleet of repositories secure by sending coding agents to do
the maintenance, gating every change behind policy, and granting autonomy
only as the work holds up. It runs unattended on an always-on box and you
attach to it when you want to look.

What it does, in order, for every run:

1. Clones the repository fresh and creates a branch.
2. Briefs an agent (Claude Code headless today) with the job and the repo's
   `AGENTS.md`, under caps on turns, dollars and tools that the agent cannot
   see or change.
3. Records every tool call and message as it happens.
4. Checks the resulting diff against policy: allowed paths, size, and a set
   of always-locked files such as release workflows and key material.
5. Commits as its own identity and opens a pull request. The agent itself
   can never commit, push or merge.

Whether that pull request then waits for you, merges after a quiet window,
or merges on its own is decided by a trust ladder per repository and class
of change. A class earns its way up with approvals and is knocked back down
by a rejection or a post-merge revert.

## Status

First cut. The command line runs one job against one repo and shows what
happened. The scheduler daemon, the TUI, auto-merge and the verifier are
the next steps, in that order.

## Try it

```
go build -o marshal .
./marshal init          # writes ~/.config/marshal/fleet.yaml
$EDITOR ~/.config/marshal/fleet.yaml
./marshal check
./marshal prompt posture-audit spotify-tui       # see the brief
./marshal run posture-audit spotify-tui --no-pr  # commit locally, open nothing
./marshal runs
./marshal show <run-id>
```

Requires `git`, `gh` (authenticated), and `claude` on PATH.

## Design rules

- The fleet file is the only source of caps, allowlists and trust levels.
  It lives outside every repository and the agent never reads or writes it.
- Deterministic work is done deterministically. The agent is for judgment.
- The repository that holds MARSHAL is itself a fleet member.
