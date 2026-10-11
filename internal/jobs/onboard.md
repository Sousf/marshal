# Onboard a repository

Bring this repository up to the baseline MARSHAL needs to maintain it. Add
only what is missing; where something exists, leave it alone and say so in
the report.

1. **Continuous integration on pull requests.** If there is no workflow that
   runs on `pull_request`, add `.github/workflows/ci.yml` that builds, runs
   the test suite, and runs the ecosystem's static checks. For Go: `go vet
   ./...`, `go test ./...`, `go build ./...`, and `govulncheck ./...`. Use
   `permissions: contents: read` at the top level. Pin every action to a
   full commit SHA with the version tag in a trailing comment; resolve SHAs
   with `gh api repos/<owner>/<repo>/git/ref/tags/<tag>` and never guess.
   Run the same commands locally first and make them pass before you
   finish. If `govulncheck` reports findings, fix them in this same
   change: bump the affected modules to their fixed versions, and for
   standard-library findings add a `toolchain` line in `go.mod` for the
   fixed Go release, which `setup-go` honours. A workflow that is red on
   the pull request that adds it is not finished.
2. **Dependency updates.** If `.github/dependabot.yml` is missing, add it for
   the package ecosystem and for `github-actions`, weekly.
3. **Conventions for agents.** If `AGENTS.md` is missing at the root, write
   one from what you can see: how to build, how to test, the layout, and
   anything an automated maintainer must not touch (release workflows,
   signing, generated files). Keep it under forty lines and factual; do not
   invent conventions the code does not show.
4. **Report what you could not do.** Branch protection and required checks
   are repository settings, not files; if `gh api repos/<owner>/<repo>/branches/<default>/protection`
   shows none, say so in the report with the settings to enable. Do not try
   to change settings.

Do not touch release or publishing workflows. Do not add dependencies.
