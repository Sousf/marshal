# Posture audit

You are hardening the security posture of this repository. Audit it against
the checklist below and fix what you can in one coherent change. Prefer the
smallest edit that closes each gap.

Checklist:

1. **GitHub Actions pinned to commit SHAs.** Every `uses:` in
   `.github/workflows/*.yml` should reference a full 40-character SHA with the
   version tag in a trailing comment, for example
   `uses: actions/checkout@<sha> # v4.2.2`. Resolve SHAs with
   `gh api repos/<owner>/<repo>/git/ref/tags/<tag>` or `git ls-remote`.
   Never guess a SHA.
2. **Least-privilege workflow permissions.** Each workflow has a top-level
   `permissions:` block with the minimum it needs, usually `contents: read`.
3. **Dependabot or Renovate configured** for the project's package ecosystem
   and for GitHub Actions, with a weekly schedule.
4. **Go projects:** `govulncheck ./...` runs in CI and is clean now. Run it
   and fix any finding that has an upstream fix available by bumping the
   module. Do not add new dependencies.
5. **Secrets.** Run `gitleaks detect --no-git` if available, or grep for
   obvious key material. Report findings; do not rewrite history.
6. **Personal information (PII).** Look through tracked files, test
   fixtures, sample data, logs and docs for real people's details: email
   addresses, phone numbers, postal addresses, national ID or social
   security numbers, payment card numbers, dates of birth, IP addresses
   tied to a person. Replace PII in fixtures and examples with reserved
   placeholders (`user@example.com`, `*.invalid`, 555-01xx phone numbers,
   documented test card numbers) and keep the tests passing. Leave contact
   details a maintainer published on purpose (LICENSE, CODEOWNERS,
   SECURITY.md, package author fields) alone. Report PII in git history or
   in files you cannot safely edit; do not rewrite history.
7. **Release provenance.** If the repo publishes releases, note whether
   artifacts are signed or attested. Report only; release workflows are
   locked and you must not edit them.

Report anything you found but did not fix, with a one-line reason each.
