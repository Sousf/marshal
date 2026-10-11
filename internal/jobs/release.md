# Release

This job runs no agent. MARSHAL prepares the release itself, in Go:

1. Find the last `v*` tag and what merged on the default branch since.
2. Propose the next version: a patch above the last tag, `0.1.0` for a
   first release, or a higher version the operator set on the release
   branch.
3. Write `VERSION` and prepend the notes to `CHANGELOG.md`.
4. Open, or update in place, the one release pull request on
   `marshal/release`.

Merging that pull request is the release: the repository's
`.github/workflows/release.yml` tags the version and publishes it.
