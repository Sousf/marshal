package main

// starterFleet is what `marshal init` writes. It is also examples/fleet.yaml;
// the test keeps the two identical.
const starterFleet = `# MARSHAL fleet file. The agent never edits this; you do.
version: 1

defaults:
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
  identity:
    name: MARSHAL
    email: marshal@users.noreply.github.com

repos:
  - name: spotify-tui
    url: git@github.com:Sousf/spotify-tui.git
    jobs: [posture-audit, vuln-fix, ci-fix]
  - name: system-patch
    url: git@github.com:Sousf/system-patch.git
    jobs: [posture-audit, vuln-fix, ci-fix]

jobs:
  - name: posture-audit
    description: pin actions, tighten permissions, dependabot, govulncheck
    schedule: weekly
    class: hardening
    paths:
      - ".github/workflows/*.yml"
      - ".github/dependabot.yml"
      - "go.mod"
      - "go.sum"
    max_diff_lines: 400
  - name: vuln-fix
    description: fix what govulncheck reports
    schedule: daily
    class: dependency
    max_diff_lines: 800
  - name: ci-fix
    description: investigate and fix a red default branch
    schedule: on-failure
    class: code
    max_usd: 5.00

policy:
  classes:
    hardening:  {start: review, promote: 3, demote_on_reject: 1, demote_on_revert: 2, soak_hours: 24}
    dependency: {start: review, promote: 3, demote_on_reject: 1, demote_on_revert: 2, soak_hours: 24}
    code:       {start: review, promote: 5, demote_on_reject: 1, demote_on_revert: 2, soak_hours: 48}
    release:    {start: locked}
  caps:
    prs_per_repo_per_day: 3
    auto_merges_per_repo_per_day: 2
    usd_per_day: 20.00
`
