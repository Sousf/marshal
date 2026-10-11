---
name: Fix govulncheck findings
description: Turn a red govulncheck into a green one by bumping modules and the Go toolchain, without adding dependencies.
learned_from: hmon/d4ad3349
---

1. Run `govulncheck -show verbose ./...` locally and read the call stacks. Only findings under "Symbol Results" (with a call stack) are reachable. Findings that appear only under "Module Results" are not called: report them with the fixed version, and do not bump for them.
2. For a reachable finding in a module, bump that module to the fixed version named in the finding: `go get <module>@<fixed>` then `go mod tidy`. Never add a module and never replace one with a fork.
3. For a reachable finding in the standard library, the fix is a newer Go patch release. First check how CI installs Go in `.github/workflows/*.yml`:
   - With `go-version-file: go.mod`, CI installs exactly what `go.mod` names. Add or raise the `toolchain go1.X.Y` line to the fixed release. A local Go that is already newer hides these findings; CI will not.
   - With `go-version: stable` (or another floating version), CI already builds with the latest release. A `toolchain` line has no effect, because it only switches to a newer Go than the one installed. Report the finding instead of editing `go.mod`.
4. Build and run the full test suite, then run `govulncheck ./...` again and confirm it exits 0. Quote the before and after in the report.
5. The command sandbox may reject `$?` in shell commands. Use `cmd && echo CLEAN || echo FINDINGS` to capture the exit status.
