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

Working today:

- `marshal run` executes one job in the foreground and opens a PR.
- `marshal serve` is the daemon: fires jobs on their schedules, watches the
  PRs they opened, moves the trust ladder when you merge or close one, and
  merges on its own only where a class has earned `soak` or `auto` and
  every gate agrees (CI green, no changes requested, no `hold` label, soak
  window elapsed, daily cap not hit, kill switch on).
- `marshal tui` shows the fleet, the runs, a live run's tool calls, and the
  ladder, and can start a job or flip the kill switch.
- The verifier: before any autonomous merge, a second agent that never saw
  the proposer's session checks out the PR head, reads only the diff and the
  claim, runs the tests, and answers PASS, FAIL or UNSURE through a fixed
  schema. Only PASS merges. Anything else holds the PR for you, posts the
  findings as a comment, and labels it `marshal:needs-human`. Deterministic
  checks the model cannot be talked out of run alongside it: a new module in
  go.mod or a changed workflow action fails the verdict on their own. The
  verdict is bound to the head SHA, so a new push re-verifies.

- Red checks on a MARSHAL PR start a follow-up run on the same branch with
  the failing logs; a reviewer's "changes requested" starts one with the
  comments. The fix is pushed to the PR and answered in a comment. Each
  failing head is attempted once, a few attempts per PR
  (`caps.check_fix_attempts`), then the PR is labelled for a human.
- Learning. A run may end its report with `## Lessons`: facts about the
  repository a future run needs. MARSHAL appends them to the repo's
  `AGENTS.md` in the same commit, so they arrive in the same PR, under the
  same review and the same verifier, and are loaded into every later run.
  The verifier treats an AGENTS.md line that reads as an instruction rather
  than a fact as a high finding. Separately, the last few outcomes of a job
  on a repo, including what a reviewer said when closing one, go into the
  next brief as history.

- Revert watch. Every merge MARSHAL made on its own is watched: once the
  default branch's CI reports on the merge commit, green settles it and red
  reverts it, as a PR the app merges at once, with the class demoted two
  steps and an urgent notification. Human merges are not watched.
- Crash recovery. On start the daemon cancels runs a previous daemon left
  "running" and removes orphaned work trees.
- Notifications. ntfy, Telegram or a webhook, chosen per event kind, plus a
  daily digest at a configured time. `marshal notify test` proves the
  channel; `marshal digest` prints the summary.

- Skills. A reviewed library of procedures under `skills/` in this repo.
  Every run gets the relevant ones in its brief, chosen by matching the job
  and repo against each skill, and the whole library mounted read-only. A
  run may end its report with `## Skill: <name>` to propose a new or
  corrected procedure; that becomes a PR in the `memory` class, which never
  merges on its own. Content that pipes downloads to a shell, handles
  credentials or weakens a check is refused before a PR is opened, and the
  verifier checks the rest.
- The operator profile. `skills/OPERATOR.md` is what MARSHAL knows about
  how you want things done. A daily `reflect` job reads the week's reviewer
  feedback, rejections and reverts and proposes edits with the evidence
  cited, again as a reviewed PR. Every brief carries the profile.
- Tasks. `marshal task "<instruction>"`, the `t` key in the interface, or a
  `tasks:` entry in the fleet file. With `--repo` it is a job whose result
  is a PR; without, the agent works in an empty scratch directory with
  read, search, web and write-to-scratch tools, no tokens, and leaves a
  report under `~/.local/share/marshal/reports` with a notification.

- The contract. A repository may carry a `marshal.yaml` naming how it is
  built and checked: `build`, `test`, `lint`, `vuln`, `format`, `smoke`,
  each a command. With it, MARSHAL measures the clean tree before the agent
  runs, puts the exact commands and the pre-existing state in the brief,
  and refuses after the run any change that breaks a verb that passed
  before. The verifier runs the same verbs and treats the results as facts
  that outrank the model's account. `locked` paths add to MARSHAL's own
  locked set; `sensitive` paths mark a proposal for human review whatever
  the trust level. This repository carries one.

- Chat. `marshal chat` hands the terminal to
  Claude Code with MARSHAL's context in its system prompt and its tools cut
  down to the `marshal` command line plus read, search and the web. From
  there you can ask what happened, start jobs and tasks, push fixes, and
  flip the kill switch, in conversation. It cannot edit files or use git or
  gh. The conversation resumes across openings; `--new` starts over.

- Pre-flight. Before any pull request is opened, MARSHAL runs the checks
  GitHub will run on it, in the checkout, after the agent's change. With
  no `marshal.yaml` it derives them from the pull-request workflow, running
  the `run:` steps in order with the Go toolchain `setup-go` would install,
  so a newer local Go cannot hide what CI will see. Failures go back to the
  agent to fix in the same checkout, twice at most, including failures that
  were already there. Nothing is opened until every check passes: your job
  is to review green pull requests, not to wait for fixes to land. A change
  that breaks something that passed before can never be opened;
  `--allow-red` lets a pre-existing failure through, nothing else.
- Spend control. Before any agent starts, the job's probe checks the
  checkout without a model: the contract's `vuln` verb or govulncheck for
  vuln-fix, unpinned actions and missing permissions or Dependabot for
  posture-audit, the baseline files for onboard, new feedback for reflect.
  Nothing to do means no run and no cost, recorded as skipped. A scheduled
  job on a repo whose default branch has not moved since it last ran is
  skipped without even cloning. Procedural jobs default to Sonnet at
  medium effort; `--force` runs the agent regardless of the probe.

- The `never:` floor. A block in the fleet file that nothing can lift: its
  `paths` are locked for every job, task and follow-up, its `tools` are
  disallowed for every agent including the verifier and chat, and its
  `auto_merge` patterns (`repo/class`, globs) hold matching proposals for
  a human at any trust level.
- Incidents. A failure is filed under a signature: repo, job and the error
  with ids, shas, temp paths, times and numbers stripped. You hear about a
  new incident once, get a daily reminder until you `marshal ack` it, and
  the next success closes it. `marshal incidents` lists them; the status
  line counts the open ones.
- Quota hold. When Claude Code stops on a usage or rate limit, MARSHAL
  reads the reset time it reports, parks every agent run until then (the
  scheduler, follow-ups, tasks and the verifier alike), says so once, and
  picks up on its own afterwards. `marshal quota` shows it, `marshal quota
  clear` lifts it early.
- Skill nudge. A run of 20 turns or more that proposed no skill gets one
  cheap, tool-less Sonnet call over a bounded digest of what it did,
  asking whether there was a procedure worth keeping. If there was, it
  becomes a reviewed skill PR like any other. `memory.nudge_turns`
  (`-1` turns it off) and `memory.nudge_model` configure it.
- Small, stable memory in briefs. The operator profile is capped at 2200
  bytes, each injected skill at 2000 and run history at 1375, with the path
  to the full text. The stable part of a brief comes first and what
  changes per run comes last, so consecutive runs share a cacheable prefix.

- Releases. The `release` job uses no agent and costs nothing. It keeps
  one "Release vX.Y.Z" pull request open per repository: `VERSION` gets a
  patch bump and `CHANGELOG.md` gets the titles of what merged since the
  last tag. The job rebuilds the PR as more merges land. To ship a minor or
  major release, edit `VERSION` on the branch; MARSHAL keeps a higher
  version you set. Merging the PR is the only way anything ships: the
  repository's `release.yml` tags the version and runs GoReleaser, which
  updates the Homebrew tap. Agents cannot edit `VERSION`, `CHANGELOG.md`
  or release workflows, and release PRs never merge on their own.

- Homebrew onboarding. With `homebrew.tap` set, onboarding checks each Go
  repo for a GoReleaser `brews` section and the standard release
  workflow. If either is missing, MARSHAL fills fixed templates from what
  it reads in the repo (main package, version variable, licence, README
  summary), checks that it cross-compiles with `CGO_ENABLED=0`, and opens
  a "Release through Homebrew" PR. Agents never write these files. The
  deploy key the workflow pushes with needs admin rights the app doesn't
  have, so you run `marshal tap-key <repo>` once. It uses your own gh
  login, and the private key goes from memory to the repo secret without
  touching disk. `marshal trigger brew-setup <repo>` runs the same check on
  its own, without onboarding's agent run.

- Dependabot triage. With a `dependabot` class in the policy, MARSHAL
  checks every repo for Dependabot PRs every 10 minutes and runs the
  verifier on each head once CI has finished. FAIL closes the PR with the
  reasons, and so does red CI the verifier couldn't pass. UNSURE, or a
  deterministic red flag (a new module, a changed Action) on an update
  whose CI is green, labels it for you. PASS with green checks gets MARSHAL's
  approval, which satisfies a one-approval ruleset. It merges when the
  class's level allows, behind the same gates as MARSHAL's own PRs: kill
  switch, caps, `never:`, soak and revert watch. With `soak`, the window
  counts from when Dependabot opened the PR.

- `marshal preflight` runs a repository's checks on a clean copy of HEAD
  the way CI will. It works in any repo and needs no fleet file. Checks
  come from `marshal.yaml`, or else from the pull-request workflows,
  without runner setup (`sudo apt-get` and the like) and without your git
  config. A workflow step whose program isn't installed is skipped and
  named; the steps after it still run. `marshal preflight --install-hook`
  makes every `git push` in that clone run it first; `git push
  --no-verify` skips it once. MARSHAL's own pre-flight runs the same way.

- Upgrades on your own machine. With `homebrew.upgrade_local: true`, a
  daemon running where the tools are installed upgrades each one as soon
  as its release lands. When it sees a release PR merge, it checks the tap
  once a minute for that version, and nothing more once it arrives. Then
  it runs `brew upgrade` for that formula if it's installed there. If the
  tool is MARSHAL, the daemon restarts into the new binary once nothing is
  running, keeping its pid. If a release takes more than two hours to
  reach the tap, you're notified and it stops checking.

[SECURITY.md](SECURITY.md) is the trust model: what is a boundary, what is
only a heuristic, and the gaps that remain until runs are isolated.

Not yet: `marshal doctor` for contract conformance and a posture score,
adapters for agents other than Claude Code, a review screen in the
interface, an inbound chat channel such as Telegram for dispatching from a
phone.

## Run it yourself

Requirements on the machine that runs it: Go 1.27+, `git`, `gh` logged in
(`gh auth status`), and `claude` logged in. Both CLIs must work for the user
that runs MARSHAL, because the daemon shells out to them.

```sh
git clone git@github.com:Sousf/marshal.git && cd marshal
go build -o ~/.local/bin/marshal .

marshal init                 # writes ~/.config/marshal/fleet.yaml
$EDITOR ~/.config/marshal/fleet.yaml   # your repos, your caps
marshal check
marshal jobs                 # effective model, effort, caps per job
marshal prompt posture-audit spotify-tui   # read the brief before you spend money
```

First run, by hand, with nothing published:

```sh
marshal run posture-audit spotify-tui --no-pr --keep
marshal runs
marshal show <run-id>        # every tool call, denials, the report
```

`--no-pr` commits on a local branch inside the kept work tree and opens
nothing. When the report looks right, drop the flags and it opens the PR.

Then the daemon and the interface:

```sh
marshal serve --tick 1m      # foreground; ctrl-c stops it
marshal                      # in another terminal: the TUI attaches
marshal status
marshal trigger vuln-fix spotify-tui
marshal autonomy off         # the kill switch
```

### Running the daemon elsewhere, controlling it from anywhere

The daemon listens on a Unix socket, never a port, so remote control is an
ssh tunnel to that socket and ssh keys are the authentication:

```sh
MARSHAL_HOST=hermes1 marshal            # the interface, against the daemon on hermes1
MARSHAL_HOST=hermes1 marshal status
MARSHAL_HOST=hermes1 marshal chat       # chat too; its commands go through the tunnel
```

Export `MARSHAL_HOST` in your shell and every command does this. One tunnel
per process, closed on exit. `marshal logs -f` is the one thing that needs
a real ssh session.

To put the daemon on a host:

```sh
cd ~/dev/marshal
marshal deploy hermes1 --dry-run        # the plan
marshal stop                            # if a local daemon holds the history
marshal deploy hermes1 --with-state     # binary, fleet file, app key, database, unit
```

It cross-compiles for linux/amd64, so Go is not needed on the host; `gh`
and `claude` are, logged in as the service user, or pass
`--env ANTHROPIC_API_KEY=...` to give the service its own key. The unit
reads `~/.config/marshal/env` for secrets and needs `loginctl
enable-linger` so it runs while you are logged out.

### Testing without spending

`go test ./...` runs with no network and no `claude`. Everything that talks
to an agent goes through an adapter interface, and the stream parser is
tested against recorded output. A cheap real run is a fleet file with
`model: claude-haiku-5-5`, `max_turns: 8`, `max_usd: 0.50` and a one-line
job; `examples/smoke-fleet.yaml` is one; a run costs about one cent.

### Trying the verifier

```sh
marshal verify <run-id>              # verify a proposal now, print the verdict
marshal verify <run-id> --comment    # and post it on the PR
```

Set `policy.verifier.always: true` to have every proposal verified and
commented, whatever its trust level. It costs a verifier run per PR and
makes your own review a read of the findings rather than of the diff.

### Giving a repository a contract

```yaml
# marshal.yaml at the repository root
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

Every verb is optional and runs through `sh` in the checkout with
credentials scrubbed from the environment. A verb whose program is not
installed is reported as unavailable, not failed.

### Adding a repository

Add an entry under `repos:` in the fleet file and run `marshal check`.
`name` is what you type everywhere else; `url` is what git clones; `jobs`
is which jobs may run against it. `branch` is only needed when the PR base
is not the remote's default branch, `github: owner/repo` only when the slug
cannot be read off the url. A new repo starts every class at `review`.

If the repo has no CI, Dependabot or `AGENTS.md`, the `onboard` job adds
what is missing and reports the branch protection settings to turn on:

```sh
marshal run onboard <repo>
```

Branch protection on the default branch, requiring a PR, is what makes
"the agent cannot push to main" true at GitHub's end as well as MARSHAL's.
Autonomy needs CI checks on PRs; without them nothing merges itself.

### Acting as a GitHub App

Without an app, MARSHAL uses your own `gh` login: its PRs look like yours
and branch protection cannot require you to review them. With an app, PRs
and merges come from `<slug>[bot]`, the agent gets a one-hour read-only
token, and the harness holds the one that writes.

Creating the app is a form at <https://github.com/settings/apps/new>. It
cannot be done from the command line. Settings:

| field | value |
|---|---|
| Name | something unique, e.g. `marshal-<yourname>`; this becomes the slug |
| Homepage URL | the MARSHAL repo |
| Webhook | untick *Active* |
| Repository permissions | Contents: read and write · Pull requests: read and write · Issues: read and write (labels) · Workflows: read and write (to add `ci.yml`) · Actions: read · Checks: read · Metadata: read |
| Where can it be installed | only on this account |

After *Create GitHub App*: note the **App ID**, click *Generate a private
key* and save the `.pem` as `~/.config/marshal/marshal-app.pem` with mode
600, then *Install App* on the repos in the fleet. The number at the end of
the URL you land on (`/settings/installations/<id>`) is the **installation
ID**. Fill in the `github_app` block in the fleet file and run
`marshal check`, which mints a token to prove it works.

For auto-merge under branch protection that requires reviews, add the app
to the rule's bypass list. Everything else still applies: the ladder, the
verifier, CI green, the caps.

### Trying autonomy safely

Autonomy only acts on PRs MARSHAL opened, only when the PR has CI checks and
they pass, and only once a class has earned its level. A fresh fleet starts
every class at `review`, so nothing merges itself until you have approved
`promote` PRs of that class in a row. `marshal trust` shows where each one
stands. `marshal autonomy off` stops every auto-merge immediately and
survives restarts.

## Design rules

- The fleet file is the only source of caps, allowlists and trust levels.
  It lives outside every repository and the agent never reads or writes it.
- Deterministic work is done deterministically. The agent is for judgment.
- The repository that holds MARSHAL is itself a fleet member.
