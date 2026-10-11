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

Not yet: `marshal doctor` for contract conformance and a posture score,
adapters for agents other than Claude Code, a review screen in the
interface, a chat channel for dispatching tasks from a phone.

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

To run it on an always-on host as a user service, see
`packaging/marshal.service`. To look at it from your laptop, ssh in and run
`marshal`; the daemon listens on a Unix socket, never a port.

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
