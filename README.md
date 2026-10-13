# MARSHAL

**M**aintains, **A**udits, **R**epairs, **S**ecures, **H**ardens, **A**nd **L**earns.

A daemon that sends coding agents to maintain a fleet of GitHub
repositories and opens their work as pull requests behind policy checks.
It runs unattended on an always-on box, and you attach to it when you want
to look.

## Install

```sh
brew install sousf/tap/marshal
marshal version
```

The formula builds for Linux and macOS, amd64 and arm64. From source,
with Go 1.27 or later:

```sh
git clone https://github.com/Sousf/marshal.git && cd marshal
go build -o ~/.local/bin/marshal .
```

The machine that runs jobs also needs `git`, `gh` logged in
(`gh auth status`) and `claude` logged in, all for the user that runs
MARSHAL. The daemon shells out to both CLIs.

## First run

```sh
marshal init                          # writes ~/.config/marshal/fleet.yaml
$EDITOR ~/.config/marshal/fleet.yaml  # your repos, your caps
marshal check                         # validates the file and the GitHub identity
marshal jobs                          # effective model, effort, caps per job
marshal prompt posture-audit <repo>   # read the brief before you spend money
```

`marshal check` prints `ok` with the count of repos, jobs and classes. Then
one run by hand, with nothing published:

```sh
marshal run posture-audit <repo> --no-pr --keep
marshal runs
marshal show <run-id>                 # every tool call, denials, the report
```

`--no-pr` commits on a local branch inside the kept work tree and opens
nothing. When the report looks right, drop both flags and the same command
opens the PR.

## Run the daemon

```sh
marshal serve --detach         # logs to ~/.local/share/marshal/serve.log
marshal                        # the TUI attaches to it
marshal status                 # is it up, is autonomy on, what is running
marshal trigger vuln-fix <repo>
marshal autonomy off           # the kill switch; survives restarts
marshal logs -f
marshal stop
```

Without `--detach`, `marshal serve` stays in the foreground until ctrl-c.
It fires jobs on their schedules, watches the PRs they opened, moves the
trust ladder when you merge or close one, and merges on its own only where
the ladder allows. In the TUI, `?` lists the keys: `1 2 3` switch between
dashboard, runs and trust, `n` starts a job, `t` a task, `a` flips
autonomy.

`marshal help` lists every command.

## What one run does

1. Clones the repository fresh and creates a branch.
2. Runs the job's probe, which needs no model. Nothing to do means no
   agent, no cost, and the run is recorded as skipped.
3. Briefs an agent (Claude Code, headless) with the job and the repo's
   `AGENTS.md`, under caps on turns, dollars and tools that the agent
   cannot see or change.
4. Records every tool call and message as it happens.
5. Checks the diff against policy: allowed paths, size, and always-locked
   files such as release workflows and key material.
6. Runs the checks CI will run (pre-flight, below). Failures go back to
   the agent.
7. Commits as its own identity and opens a pull request. The agent itself
   can never commit, push or merge.

## Autonomy and the trust ladder

Each repository and class of change sits on a ladder: `locked`, `review`,
`soak`, `auto`. At `review` the PR waits for you. At `soak` it merges after
a quiet window (`soak_hours`). At `auto` it merges once every gate agrees.
A class starts at its `start` level and climbs one step after `promote`
human approvals in a row; `promote: 0` means it never climbs. A rejection
or a post-merge revert knocks it back down. Machine merges never count as
approvals. `marshal trust` shows where each class stands.

An autonomous merge needs all of these:

- CI checks exist on the PR and pass. A repo without CI gets no autonomy.
- A verifier PASS bound to the PR's current head.
- No changes requested and no `hold` label.
- The soak window has elapsed, where the level has one.
- The daily caps in the fleet file are not hit.
- The kill switch is on.
- No `never.auto_merge` pattern matches.

**The verifier.** A second agent that never saw the proposer's session
checks out the PR head, reads only the diff and the claim, runs the tests,
and answers PASS, FAIL or UNSURE through a fixed schema. Only PASS merges.
Anything else holds the PR, posts the findings as a comment and labels it
`marshal:needs-human`. Deterministic checks run alongside the model and
outrank it: a new module in `go.mod` or a changed workflow action fails the
verdict on its own. A new push re-verifies.

```sh
marshal verify <run-id>              # verify a proposal now, print the verdict
marshal verify <run-id> --comment    # and post it on the PR
```

`policy.verifier.always: true` verifies and comments on every proposal,
whatever its level. That costs a verifier run per PR, and your own review
becomes a read of the findings.

**Revert watch.** Every merge MARSHAL made on its own is watched until the
default branch's CI reports on the merge commit. Green settles it. Red
reverts it through a PR the app merges at once, demotes the class two
steps, and sends an urgent notification. Human merges are never watched or
reverted.

**The `never:` floor.** A fleet file block that nothing can lift. Its
`paths` are locked for every job, task and follow-up. Its `tools` are
disallowed for every agent, the verifier and chat included. Its
`auto_merge` patterns (`repo/class` globs) hold matching proposals for a
human at any level.

## Pre-flight and the contract

No pull request opens until every check passes. MARSHAL runs the checks
GitHub will run, in the checkout, after the agent's change. Failures go
back to the agent to fix in the same checkout, twice at most, including
failures that were already there. A change that breaks something that
passed before is never opened; `--allow-red` on `marshal run` lets a
pre-existing failure through and nothing else.

With no contract, the checks come from the pull-request workflows: the
`run:` steps in order, with the Go toolchain `setup-go` would install, so a
newer local Go cannot hide what CI will see.

A contract is a `marshal.yaml` at the repository root:

```yaml
contract: 1
verbs:
  build:  go build ./...
  test:   go test ./...
  lint:   go vet ./...
  vuln:   govulncheck ./...
  format: test -z "$(gofmt -l .)"
  smoke:  ./bin/app --version
locked:    [deploy/**]
sensitive: [internal/auth/**]
```

Every verb is optional and runs through `sh` in the checkout, with
credentials and the host's git identity scrubbed. A verb whose program is
not installed is reported as unavailable. MARSHAL measures the clean tree
first, puts the commands and their state in the brief, and refuses any
change that breaks a verb that passed. The verifier runs the same verbs
and treats the results as facts. `locked` paths add to MARSHAL's own locked
set; `sensitive` paths send a proposal to a human whatever the level. This
repository's `marshal.yaml` is the reference.

The same checks run on your own machine, in any repo, with no fleet file:

```sh
marshal preflight                   # CI's checks on a clean copy of HEAD
marshal preflight --install-hook    # run them before every git push
```

Runner setup such as `sudo apt-get` is skipped. A step whose program is not
installed is skipped and named, and the steps after it still run.
`git push --no-verify` skips the hook once.

## Follow-ups on open PRs

Red checks on a MARSHAL PR start a follow-up run on the same branch with
the failing logs. Any reply from you short of an approval (a comment, a
review, an inline note) starts one with what you said, on skill PRs too.
The fix is pushed to the PR and answered in a comment that gives every
point an outcome: done, not done and why, or answered. If no follow-up can
run, the comment says why.

A PR that GitHub reports as conflicting gets a follow-up before anything
else. MARSHAL merges the base into the branch, the agent resolves the
files git could not, and MARSHAL pushes a merge commit. Policy checks the
PR's diff against the base, so locked files the base changed do not
block it. A run that leaves a conflict marker or aborts the merge pushes
nothing.

Each failing or conflicting head is attempted once, and each PR at most
`caps.check_fix_attempts` times (default 2, `-1` turns follow-ups off).
After that the PR is labelled for a human. `marshal fix <run-id>` pushes a
fix now, outside the cap.

## Memory

- **Lessons.** A run may end its report with `## Lessons`: facts about the
  repository a future run needs. MARSHAL appends them to the repo's
  `AGENTS.md` in the same commit, so they go through the same review and
  verifier, and every later run loads them. The verifier treats a line that
  reads as an instruction as a high finding.
- **Skills.** A reviewed library of procedures under `skills/` in the
  memory repo. Each brief gets the skills that match its job and repo, and
  the whole library is mounted read-only. A run proposes a new or corrected
  one with `## Skill: <name>`, which becomes a PR in the `memory` class.
  That class never merges on its own. Content that pipes downloads to a
  shell, handles credentials or weakens a check is refused before a PR
  exists.
- **The operator profile.** `skills/OPERATOR.md` is how you want things
  done. A daily `reflect` job reads the week's reviewer feedback,
  rejections and reverts and proposes edits with the evidence cited, as a
  reviewed PR. Every brief carries it.
- **Skill nudge.** A run of 20 turns or more that proposed no skill gets
  one tool-less Sonnet call over a digest of what it did, asking whether a
  procedure was worth keeping. `memory.nudge_turns` (`-1` turns it off) and
  `memory.nudge_model` configure it.
- **History.** The last few outcomes of a job on a repo, including what a
  reviewer said when closing one, go into the next brief, labelled as data.

Briefs cap memory: the operator profile at 2200 bytes, each skill at 2000,
history at 1375, each with the path to the full text. The stable part of a
brief comes first and the per-run part last, so consecutive runs share a
cacheable prefix.

## Spend

Probes run before any agent: the contract's `vuln` verb or govulncheck for
`vuln-fix`, unpinned actions, missing permissions or Dependabot, and
likely PII (real email addresses, SSNs, card numbers) for `posture-audit`,
the baseline files for `onboard`, new feedback for `reflect`. A scheduled job on a repo whose default branch has not moved
since its last run is skipped without cloning. Procedural jobs default to
Sonnet at medium effort. `--force` runs the agent regardless of the probe.

When Claude Code stops on a usage or rate limit, MARSHAL reads the reset
time, parks every agent run until then (scheduler, follow-ups, tasks and
verifier alike), says so once, and resumes on its own. `marshal quota`
shows the hold; `marshal quota clear` lifts it early.

## Tasks and chat

```sh
marshal task "<instruction>" --repo <repo>   # ends in a PR
marshal task "<instruction>"                 # ends in a report
```

Tasks also come from the `t` key in the TUI and from `tasks:` entries in
the fleet file, which take a schedule. Without `--repo` the agent gets an
empty scratch directory, read, search, web and write-to-scratch tools, and
no tokens. It leaves a report under `~/.local/share/marshal/reports` and
sends a notification.

`marshal chat` hands the terminal to Claude Code with MARSHAL's context in
its system prompt and its tools cut to the `marshal` command line plus
read, search and the web. Ask what happened, start jobs and tasks, push
fixes or flip the kill switch. It cannot edit files or use git or gh. The
conversation resumes across openings; `--new` starts over.

## Releases and Homebrew

The `release` job is Go code and costs nothing. It keeps one "Release
vX.Y.Z" PR open per repository: `VERSION` gets a patch bump and
`CHANGELOG.md` the titles of what merged since the last tag, rebuilt as
more merges land. For a minor or major release, edit `VERSION` on the
branch; MARSHAL keeps a higher version you set. Merging the PR is the only
way anything ships: the repo's `release.yml` tags the version and runs
GoReleaser, which updates the tap. Agents cannot edit `VERSION`,
`CHANGELOG.md` or release workflows, and release PRs never merge on their
own.

With `homebrew.tap` set, onboarding checks each Go repo for a GoReleaser
`brews` section and the standard release workflow. If either is missing,
MARSHAL fills fixed templates from the repo (main package, version
variable, licence, README summary), checks that it cross-compiles with
`CGO_ENABLED=0`, and opens a "Release through Homebrew" PR. Agents never
write these files. `marshal trigger brew-setup <repo>` runs the check
without onboarding's agent run.

The workflow pushes to the tap with a deploy key, and installing one needs
admin rights the app doesn't have. Run this once per repo:

```sh
marshal tap-key <repo>
```

It uses your own gh login, and the private key goes from memory to the
repo secret without touching disk.

With `homebrew.upgrade_local: true`, a daemon on the machine where the
tools are installed upgrades each one when its release lands. After a
release PR merges it checks the tap once a minute for that version, then
runs `brew upgrade` for the formula. If the tool is MARSHAL, the daemon
restarts into the new binary once nothing is running, keeping its pid. A
release that takes more than two hours to reach the tap sends a
notification and ends the wait.

## Dependabot triage

With a `dependabot` class in the policy, MARSHAL checks every repo for
Dependabot PRs every 10 minutes and runs the verifier on each head once CI
has finished.

| Verdict and CI                              | Result                       |
| ------------------------------------------- | ---------------------------- |
| FAIL                                        | closed, with the reasons     |
| red CI and no PASS                          | closed, with the reasons     |
| UNSURE, or a deterministic flag on green CI | labelled for you             |
| PASS and green checks                       | approved by MARSHAL          |

MARSHAL's approval satisfies a one-approval ruleset. The PR merges when
the class's level allows, behind the same gates as MARSHAL's own PRs. With
`soak`, the window counts from when Dependabot opened the PR. Closing one
never moves the ladder.

## Reviewing people's pull requests

```yaml
review:
  enabled: true
  approve_authors: [your-github-login]
```

With this in the fleet file, MARSHAL checks every repo for open pull
requests that people opened every 5 minutes. Drafts wait until they are
ready. Once CI has finished on a head, the verifier reviews it and posts
its verdict as a comment.

| Verdict and CI                            | Result                              |
| ----------------------------------------- | ----------------------------------- |
| PASS, green checks, author in the list    | approved by MARSHAL                 |
| PASS, anyone else                         | the verdict comment only            |
| FAIL                                      | changes requested, with the reasons |
| UNSURE, or a deterministic flag           | labelled for you                    |

Each head gets one review. MARSHAL never pushes to, merges or closes a
person's PR on its own, and none of this moves the trust ladder. Its
approval satisfies a one-approval ruleset, which is why only
`approve_authors` get one, and only while autonomy is on.

Only PRs from people with write access (owner, member or collaborator) are
reviewed. The verifier runs a PR's tests on the host, and until runs are
isolated that would let a stranger's code read MARSHAL's credentials; see
[SECURITY.md](SECURITY.md).

To close a PR on a fleet repo, from the shell or from `marshal chat`:

```sh
marshal close <run-id|pr-url> "superseded by #12"
```

The reason is posted on the PR.

## Running it day to day

- **Incidents.** A failure is filed under a signature: repo, job and the
  error with ids, shas, temp paths, times and numbers stripped. A new
  incident notifies once, then daily until you `marshal ack` it, and the
  next success closes it. `marshal incidents` lists them; the status line
  counts the open ones.
- **Notifications.** ntfy, Telegram or a webhook, chosen per event kind,
  plus a daily digest at a set time. `marshal notify test` sends one;
  `marshal digest` prints the summary.
- **Crash recovery.** On start the daemon cancels runs a previous daemon
  left running and removes orphaned work trees.

## Control it from another machine

The daemon listens on a Unix socket only. Remote control is an ssh tunnel
to that socket, and your ssh keys are the authentication:

```sh
export MARSHAL_HOST=<host>
marshal            # the TUI, against the daemon on <host>
marshal status
marshal chat       # its commands go through the tunnel too
```

The client needs only `marshal` and an `ssh <host>` that logs in without a
prompt. Run history, the ladder and the fleet come from the daemon, with
the fleet's credentials left out. Each process opens one tunnel and closes
it on exit. `check`, `run`, `verify` and `deploy` act on the machine you
type them on and read the local fleet file. `marshal logs` prints the last
100 lines over the tunnel; `-f` needs a real ssh session.

## Deploy to an always-on host

Run these from inside the MARSHAL source checkout, which deploy builds
from:

```sh
marshal deploy <host> --dry-run       # print the plan
marshal stop                          # if a local daemon holds the history
marshal deploy <host> --with-state    # binary, fleet file, app key, database, unit
```

Deploy cross-compiles for linux/amd64 and installs a systemd user service.
With `--brew` the host installs MARSHAL from the tap instead, and an hourly
`marshal-upgrade` timer runs `brew upgrade` outside the daemon's sandbox.

The host needs `go` for the verifier to test Go repositories, and `gh` and
`claude` logged in as the service user. `--env ANTHROPIC_API_KEY=...` gives
the service its own key. The unit reads secrets from
`~/.config/marshal/env`. Run `loginctl enable-linger` on the host so the
service keeps running while you are logged out.

Check it took: `MARSHAL_HOST=<host> marshal status` reports the daemon up.

## Add a repository

Add an entry under `repos:` in the fleet file and run `marshal check`.

| Field            | Meaning                                                   |
| ---------------- | --------------------------------------------------------- |
| `name`           | what you type everywhere else                             |
| `url`            | what git clones                                           |
| `jobs`           | which jobs may run against it                             |
| `branch`         | only when the PR base is not the remote's default branch  |
| `github`         | `owner/repo`, only when it cannot be read off the url     |

If the repo has no CI, Dependabot or `AGENTS.md`, the `onboard` job adds
what is missing and reports the branch protection settings to turn on:

```sh
marshal run onboard <repo>
```

Branch protection on the default branch, requiring a PR, makes "the agent
cannot push to main" true at GitHub's end as well as MARSHAL's.

## Act as a GitHub App

Without an app, MARSHAL uses your own `gh` login: its PRs look like yours,
and branch protection cannot require you to review them. With an app, PRs
and merges come from `<slug>[bot]`, the agent gets a one-hour read-only
token, and the harness holds the one that writes.

```sh
marshal app create     # opens a browser; confirm twice
marshal app status     # which repositories the installation reaches
```

`marshal app create` needs a browser on the same machine. It names the app
`marshal-<your login>` (`--name` overrides), asks for the least
permissions MARSHAL needs, saves the private key next to the fleet file
with mode 600, writes the `github_app` block, and mints a token to prove
it works. On the install page, pick the repos in the fleet; `marshal app
status` flags any fleet repo the installation does not cover.

For auto-merge under branch protection that requires reviews, add the app
to the rule's bypass list. The ladder, the verifier, green CI and the caps
still apply.

## Sandbox runs

Install bubblewrap (`apt install bubblewrap`), then add to the fleet file:

```yaml
isolation:
  mode: bwrap
  read_only: []   # extra paths under $HOME a run needs, e.g. ~/sdk/go1.24
  writable: []    # extra caches shared between runs
```

Check it with `marshal check`, which prints an `isolation:` line. With
`mode: bwrap`, MARSHAL refuses to start if bubblewrap cannot create a
sandbox on this host. If that happens on a recent Ubuntu, the usual cause
is AppArmor's restriction on unprivileged user namespaces
(`kernel.apparmor_restrict_unprivileged_userns`).

Each agent run and each contract verb then sees the host read-only, with
your home directory, MARSHAL's config and state directories,
`$XDG_RUNTIME_DIR`, the fleet file and the app key replaced by empty
mounts. Writable: the run's clone (its `.git` stays read-only), a private
`/tmp`, and the Go build and module caches. Read-only holes: PATH entries
under `$HOME`, `GOROOT`, the claude binary, and the memory repo's skills.
The agent also gets `~/.claude` read-write, because that is where its login
lives and refreshes. Network is not restricted yet; see `SECURITY.md`.

## Testing without spending

`go test ./...` runs with no network and no `claude`. Everything that
talks to an agent goes through an adapter interface, and the stream parser
is tested against recorded output.

For a cheap real run, use `examples/smoke-fleet.yaml`: Haiku, 8 turns, a
$0.50 cap and a one-line job. A run costs about one cent.

## Not yet

`marshal doctor` for contract conformance and a posture score, adapters
for agents other than Claude Code, a review screen in the TUI, and an
inbound chat channel such as Telegram for dispatching from a phone.

## Design rules

- The fleet file is the only source of caps, allowlists and trust levels.
  It lives outside every repository and the agent never reads or writes it.
- Deterministic work is done deterministically. The agent is for judgment.
- The repository that holds MARSHAL is itself a fleet member.

[SECURITY.md](SECURITY.md) is the trust model: what is a boundary, what is
only a heuristic, and the gaps that remain until runs are isolated.
