---
name: Fix govulncheck findings
description: Turn a red govulncheck into a green one by bumping modules and the Go toolchain, without adding dependencies.
tags: [go, govulncheck, dependencies, security]
learned_from: system-patch/b9e9c966
---

1. Run `govulncheck ./...` locally and read the call stacks. Only findings
   with a call stack are reachable; report unreachable ones, do not chase
   them.
2. For a finding in a module, bump that module to the fixed version named
   in the finding: `go get <module>@<fixed>` then `go mod tidy`. Never add a
   module and never replace one with a fork.
3. For a finding in the standard library, the fix is a newer Go patch
   release. CI installs exactly what `go.mod` names when it uses
   `go-version-file: go.mod`, so add or raise the `toolchain go1.X.Y` line
   in `go.mod` to the fixed release. A local Go that is already newer hides
   these findings; CI will not.
4. Build and run the full test suite, then run `govulncheck ./...` again
   and confirm it exits 0. Quote the before and after in the report.
