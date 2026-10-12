package main

// starterFleet is what `marshal init` writes. It is also examples/fleet.yaml;
// the test keeps the two identical.
const starterFleet = `# MARSHAL fleet file. The agent never edits this; you do.
version: 1

defaults:
  # GitHub logins (or org/team) asked to review every PR MARSHAL opens.
  # reviewers: [your-login]
  model: claude-opus-5-5
  effort: high
  max_turns: 60
  max_usd: 3.00
  # acceptEdits lets the agent write files in its clone without a prompt.
  # Shell commands still have to match allowed_tools.
  permission_mode: acceptEdits
  allowed_tools:
    - Read
    - Grep
    - Glob
    - Edit
    - Write
    - WebFetch
    - WebSearch
    - "Bash(go:*)"
    - "Bash(gofmt:*)"
    - "Bash(govulncheck:*)"
    - "Bash(staticcheck:*)"
    - "Bash(gitleaks:*)"
    - "Bash(git status:*)"
    - "Bash(git diff:*)"
    - "Bash(git log:*)"
    - "Bash(git ls-remote:*)"
    - "Bash(gh api:*)"
    - "Bash(gh run:*)"
    - "Bash(gh repo view:*)"
    - "Bash(cat:*)"
    - "Bash(ls:*)"
    - "Bash(grep:*)"
    - "Bash(find:*)"
  disallowed_tools:
    - "Bash(git commit:*)"
    - "Bash(git push:*)"
    - "Bash(git remote:*)"
    - "Bash(gh pr:*)"
    - "Bash(gh release:*)"
    - "Bash(curl:*)"
    - "Bash(wget:*)"
    - "Bash(sudo:*)"
  # Commit identity when no GitHub App is configured; with one, commits are
  # attributed to the app. The domain is reserved
  # by RFC 2606 so it can never resolve to a GitHub account: an address of
  # the form <name>@users.noreply.github.com would credit whoever owns that
  # username, which happened once and is not happening again.
  identity:
    name: MARSHAL
    email: marshal@marshal.invalid

repos:
  - name: marshal
    url: git@github.com:Sousf/marshal.git
    jobs: [onboard, posture-audit, vuln-fix, ci-fix, reflect]
  - name: spotify-tui
    url: git@github.com:Sousf/spotify-tui.git
    jobs: [onboard, posture-audit, vuln-fix, ci-fix]
  - name: system-patch
    url: git@github.com:Sousf/system-patch.git
    jobs: [onboard, posture-audit, vuln-fix, ci-fix]
  - name: arch-rice
    url: git@github.com:Sousf/arch-rice.git
    jobs: [onboard, posture-audit, ci-fix]
  - name: hmon
    url: git@github.com:Sousf/hmon.git
    jobs: [onboard, posture-audit, vuln-fix, ci-fix]

jobs:
  # Jobs may touch the whole repository except the always-locked paths
  # (release workflows, signing config, key material). A job can be confined
  # with a paths: list of globs, but a confined job cannot fix what its own
  # change breaks elsewhere, so none of these are.
  # Model and effort per job. Procedural jobs run on Sonnet at medium
  # effort, which does the work at under half the cost; judgment-heavy
  # jobs (ci-fix, follow-ups, tasks) keep the fleet default. A job's probe
  # runs first: if it finds nothing to do, no agent is started at all.
  - name: onboard
    description: add CI on pull requests, dependabot, AGENTS.md and a contract where missing
    schedule: manual
    class: hardening
    model: claude-sonnet-5-5
    effort: medium
    max_turns: 30
    max_diff_lines: 400
  - name: posture-audit
    description: pin actions, tighten permissions, dependabot, govulncheck
    schedule: weekly
    class: hardening
    model: claude-sonnet-5-5
    effort: medium
    max_turns: 30
    max_diff_lines: 400
  - name: vuln-fix
    description: fix what govulncheck reports
    schedule: daily
    class: dependency
    model: claude-sonnet-5-5
    effort: medium
    max_turns: 30
    max_diff_lines: 800
  - name: ci-fix
    description: investigate and fix a red default branch
    schedule: on-failure
    class: code
    max_usd: 5.00
  # Deterministic, no agent: keeps one "Release vX.Y.Z" PR open per repo
  # with the version bumped and the notes written. Needs a release workflow
  # that tags VERSION when it changes on the default branch.
  - name: release
    description: prepare the next release as a pull request
    schedule: daily
    class: release
  - name: reflect
    description: update the operator profile and skills from the week's reviewer feedback
    schedule: daily
    class: memory
    model: claude-sonnet-5-5
    effort: medium
    max_turns: 30

policy:
  classes:
    hardening:  {start: review, promote: 3, demote_on_reject: 1, demote_on_revert: 2, soak_hours: 24}
    dependency: {start: review, promote: 3, demote_on_reject: 1, demote_on_revert: 2, soak_hours: 24}
    code:       {start: review, promote: 5, demote_on_reject: 1, demote_on_revert: 2, soak_hours: 48}
    # Dependabot's PRs. With this class present MARSHAL triages them: the
    # verifier reads each head; FAIL closes it; PASS with green checks gets
    # MARSHAL's approval, and merges once the level allows. Soak counts from
    # when Dependabot opened the PR, so a release that turns out to be
    # compromised has time to be yanked first.
    dependabot: {start: soak, promote: 0, demote_on_revert: 2, soak_hours: 48}
    # Release PRs: prepared by MARSHAL without an agent, merged only by you.
    release:    {start: review, promote: 0}
    # Memory: skills and the operator profile. promote 0 means it never
    # merges on its own, however many you approve.
    memory:     {start: review, promote: 0}
  caps:
    prs_per_repo_per_day: 3
    auto_merges_per_repo_per_day: 2
    usd_per_day: 20.00
  # The verifier reads a proposal's diff before MARSHAL merges it on its own.
  # It never sees the proposer's session. Set always: true to run it on every
  # PR and have its findings posted for your own review too.
  verifier:
    model: claude-sonnet-5-5
    effort: medium
    max_turns: 25
    max_usd: 2.00
    always: false

# The skills library and operator profile live in this fleet repo under
# skills/. Every run reads them; proposals arrive as PRs to that repo.
memory:
  repo: marshal
  dir: skills
  # A run this long that proposed no skill is asked, by a cheap model with
  # no tools, whether it taught one. -1 turns it off.
  # nudge_turns: 20
  # nudge_model: claude-sonnet-5-5

# The Homebrew tap every tool publishes to. With it, onboarding checks that
# each Go repo releases there and, if not, opens a PR wiring it up (from
# templates, without an agent). Then run "marshal tap-key <repo>" once.
# homebrew:
#   tap: Sousf/homebrew-tap
#   upgrade_local: true   # on the machine the tools are installed on

# The deny floor. Nothing a job, task, chat, contract or trust level says
# can lift it: paths are locked everywhere, tools are disallowed for every
# agent, auto_merge (repo/class globs) always waits for a human.
# never:
#   paths: ["deploy/**"]
#   tools: ["Bash(docker:*)"]
#   auto_merge: ["marshal/*"]

# Scheduled free-form tasks. With repo: a PR; without: a report under
# ~/.local/share/marshal/reports, and a notification.
# tasks:
#   - name: weekly-deps
#     schedule: weekly
#     prompt: Compare the dependency versions across every repo in the fleet and report drift.

# Where to hear about events. Any subset; events defaults to everything but
# merged; digest is a local time of day.
# notify:
#   ntfy: {url: https://ntfy.sh/your-private-topic}
#   telegram: {bot_token: "123:abc", chat_id: "123456"}
#   webhook: https://example.invalid/hook
#   events: [proposed, needs-human, failed, reverted, digest]
#   digest: "08:00"

# Act as a GitHub App instead of your own gh login, so PRs and merges carry
# the app's identity and branch protection can require your review of them.
# Create the app at https://github.com/settings/apps/new (see README), install
# it on the repos above, save its private key, then fill this in.
# github_app:
#   app_id: 123456
#   installation_id: 7891011
#   private_key_file: ~/.config/marshal/marshal-app.pem
#   slug: marshal-sousf
`
