# Changelog

## v0.1.4 (2026-10-11)

- Act on any reply to a MARSHAL PR, and always answer
- Stop loading MCP servers into agent runs; prune old event logs (#12)

## v0.1.3 (2026-10-11)

- Operator requests skip the PR cap; refusals reach the interface
- Drop c (chat) from the interface

## v0.1.2 (2026-10-11)

- O opens every pull request waiting on you

## v0.1.1 (2026-10-11)

- brew-setup can be triggered on its own (#6)

## v0.1.0 (2026-10-11)

- MIT licence
- Dependabot: close an update whose CI fails and the verifier did not pass
- Verifier: default work root; no verdict when it never started
- marshal preflight: CI's checks, locally, before a push
- Dependabot triage, and fixes from releasing ears
- CI is green again; release PRs need a green default branch
- Fixes from onboarding ears
- Release job: only where merging the PR would ship
- Onboarding wires a repo into the Homebrew tap
- Release by merging the release PR
- Release job: one release PR per repo, prepared without an agent
- SECURITY.md: the trust model, and the gaps it admits
- Skill nudge after long runs
- Frozen memory: capped and first in the brief
- Deny floor, incident dedup, quota hold
- Remote control over ssh, and marshal deploy
- Pre-flight: every check passes before a pull request exists
- Spend: probes before agents, skip unchanged repos, Sonnet for routine jobs
- marshal chat: Claude Code as MARSHAL's conversation window
- Skill: Fix govulncheck findings (#4)
- Starter fleet: hmon (Go; all four jobs)
- The contract: marshal.yaml, baseline, gate, verifier verbs
- Starter fleet: arch-rice (dotfiles; onboard, posture-audit, ci-fix)
- Scan: dead code out, two leaks closed, less work per tick, deps bumped
- Skills, an operator profile, and free-form tasks
- Crash recovery, revert watch, notifications and a daily digest
- Follow-ups finish the job; blocked attempts do not burn the cap; marshal fix
- Interface: list navigation fixed, live log pane; follow-ups span the repo
- Learning loop, first version, and review follow-ups
- Fix failing checks on MARSHAL's own pull requests
- Commits are attributed to the GitHub App whenever one is configured
- Interface: animate list spinners, bracket tab keys, show hashes as is
- CI: run on master too
- marshal app status: which repositories the installation reaches
- marshal app create: the GitHub App manifest flow
- Default commit identity: a reserved domain, never a GitHub noreply form
- Daemon lifetime, staggered starts, detach mode
- Onboarding job, MARSHAL in its own fleet, GitHub App identity
- Verifier: a second agent before any autonomous merge
- Interface: banner, layout, keyboard polish
- Daemon and interface
- Initial cut: run one job against one repo, end to end
