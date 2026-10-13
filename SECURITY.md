# Security

MARSHAL runs unattended with credentials that can push code and merge pull
requests into repositories people run. This file says what actually stands
between a hostile input and those credentials, and what only looks like it
does.

## Reporting

The repository is private. Report a vulnerability to the owner through a
GitHub security advisory on `Sousf/marshal`, or privately to the owner
directly. Please do not open a public issue.

## The trust model in one paragraph

Only an operating system boundary is a security boundary. Everything MARSHAL
does inside its own process (tool allowlists, locked paths, the forbidden
content scan, the verifier, the trust ladder) is a heuristic. Each one
raises the cost of a mistake or an attack and catches the honest error, but
none of them can stop code that is already running as the daemon's user.
The design goal is per-run OS isolation. Until that exists, the host and
the user MARSHAL runs as are the boundary, and should be chosen with that in
mind.

## What is untrusted

Everything the agent reads or writes. That includes repository contents,
dependency changelogs, issue and PR comments, CI logs, web pages, the
agent's own report, its diff, its PR body, its structured verdict, skill
proposals, and the transcript the skill nudge reads. MARSHAL parses,
validates and bounds all of it, and marks it as data wherever it goes into
a prompt. It is never interpolated into a shell command.

## Boundaries: these hold against a hostile agent

- **GitHub's server side.** Rulesets on default branches (PR plus one
  approval) are enforced by GitHub, not by MARSHAL. A pull request is the
  only way code reaches a default branch unless an identity is on the bypass
  list. See the gap below about the app being on that list.
- **Token scope.** With a GitHub App configured, the agent's environment
  gets an installation token minted read-only (`contents`, `metadata`,
  `pull_requests`, `actions`, `checks`: read). The write token stays in the
  harness's environment for clone, push and PR calls. GitHub enforces the
  scopes, so a leaked agent token cannot write.
- **Token lifetime.** Installation tokens expire within an hour. Tokens live
  in process environments for one invocation and are never written to disk,
  logs, events, PR bodies or errors.
- **The fleet file.** Caps, allowlists, trust levels and the `never:` floor
  are read by Go from a file outside every repository. Nothing the model says
  changes them. *Caveat:* this holds only while the agent cannot write to
  that file, which today means only while it cannot run arbitrary code (see
  below).

## Heuristics: these catch mistakes and raise the bar, nothing more

- **Tool allowlists and `never.tools`.** Claude Code enforces them, which
  stops an agent that follows the rules. It does not stop one that finds a
  way round: `Bash(go:*)` permits `go run` and `go test`, and both execute
  arbitrary code. So does any allowed build tool. `WebFetch` can carry data
  out in a URL.
- **Locked paths and `never.paths`.** Checked on the finished diff before
  anything is committed. They are reliable for what MARSHAL commits. They do
  nothing about what a process does on the host while the run is going on.
- **The forbidden-content scan** on skills and memory. Regexes for things
  like piped downloads and "skip the tests". They catch the obvious; a
  determined injection rephrases.
- **The verifier.** A second model reads only the diff. It caught a PR-body
  injection on a real run, and it can also be fooled. No merge happens
  without its PASS on the exact head, so it can only block a merge, never
  cause one.
- **The trust ladder, caps, soak window and kill switch.** These limit how
  much harm gets through and how fast. They are policy, not isolation.

## Known gaps today

1. **No per-run isolation by default.** Without `isolation.mode: bwrap` in
   the fleet file (see "What the sandbox closes" below), the agent, the
   repository's own tests and the
   pre-flight checks (`go test`, the contract's verbs, derived CI commands)
   all run as the daemon's OS user, with that user's home directory and
   network. Code in a repository or in one of its dependencies, or an agent
   steered into `go run`, can read anything that user can read:
   - the GitHub App private key (`~/.config/marshal/marshal-app.pem`), from
     which it can mint a full write token,
   - the Claude Code credentials,
   - any ssh keys or `gh` login on the host.
   It can also edit the fleet file. Contract verbs run with the token
   variables scrubbed from their environment, which keeps tokens out of
   reach but leaves files on disk readable.
2. **The app is on the ruleset bypass list** so that trusted classes can
   merge without a human approval. Combined with gap 1, a stolen key means a
   direct push to a default branch.
3. **The agent has egress.** WebFetch and any allowed tool that opens a
   socket can reach the internet.

## Deploying with the gaps in mind

Until per-run isolation lands:

- Run MARSHAL as a dedicated, unprivileged OS user on a dedicated host, not
  on a workstation and not as your login user. That user should hold no ssh
  keys or credentials other than MARSHAL's own: the app key, the Claude
  login or API key, and the notify tokens.
- Give the daemon its own `ANTHROPIC_API_KEY` with a spend limit set in the
  console, rather than a personal subscription login.
- Install the GitHub App only on the fleet's repositories, not on "all
  repositories".
- Leave the app off the ruleset bypass list unless you want autonomous
  merges for a repository. Without bypass, every change waits for your
  approval.
- Use `never.auto_merge` for any repository whose compromise would hurt
  others, such as anything that publishes releases. Use `never.tools` for
  anything the fleet should never touch.
- Treat a skill or memory pull request like a change to every repository at
  once. That is what it is.
- `review:` runs the tests of pull requests people open, on this host.
  MARSHAL only does so for people with write access to the repository,
  since a stranger's code would meet gap 1. Anyone in `approve_authors`
  can get a pull request approved by a model's PASS, which satisfies a
  one-approval ruleset: list only people you would trust to merge alone.

## What the sandbox closes

With `isolation.mode: bwrap`, every agent invocation (`runner.Claude`, so
jobs, tasks, follow-ups, the verifier and the skill nudge) and every
contract verb (baseline, pre-flight, the verifier's runs) runs under
bubblewrap. `internal/sandbox` holds the mount layout. Inside:

- The root filesystem is read-only. The home directory, MARSHAL's config
  and state directories and `$XDG_RUNTIME_DIR` are empty tmpfs mounts, and
  the fleet file and app key are masked wherever they live. The database,
  the daemon socket, other runs' clones, ssh keys and a `gh` login are out
  of reach.
- The run's clone is writable and its `.git` is read-only, so a run cannot
  plant a hook or a `core.fsmonitor` that the harness's own git, which runs
  outside the sandbox with the write token, would execute.
- Its own pid, ipc, uts and user namespaces, a private `/tmp`, a new
  session, and death with the daemon.

What stays open:

- **Network.** The sandbox shares the host network (gap 3).
- **Claude credentials.** `~/.claude` is bound read-write for the agent,
  since an OAuth login refreshes its token there. An agent can read it,
  and anything it writes there persists.
- **Shared caches.** The Go build and module caches are read-write and
  shared between runs, so one run can poison what a later one compiles.
- **Harness-side tools.** `go mod download` and `go list` in the verifier,
  `govulncheck` in the probe and `go build` in brew setup run outside the
  sandbox. They read untrusted repositories but do not execute their code.
- **Chat.** `marshal chat` runs claude unsandboxed in the operator's
  terminal.
- The process runs as the same uid. A kernel bug in user namespaces is a
  way out; a separate uid would add a second layer.

## The plan for the rest

Network goes through an allowlist proxy (the module proxy, GitHub's API
read-only, the model API). The app key moves into a separate signer process
that mints tokens on request and never shares the file. Per-run copies of
`~/.claude` and the caches end the cross-run persistence above. When those
land, gap 3 closes and gap 2 depends only on the signer.

## Floors that only get stricter

Some rules may only ever be tightened in a change, never loosened:
`policy.alwaysLocked`, the verifier's deterministic fatal checks, no merge
without a PASS on the exact head, the kill switch, `never:`, and the memory
class never promoting. A change that loosens one of them needs a reason in
the commit message and the operator's explicit agreement.
