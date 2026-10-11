---
name: Fix govulncheck findings
description: Turn a red govulncheck into a green one by bumping modules and the Go toolchain, without adding dependencies.
learned_from: hmon/140dcbb7
---

1. Run `govulncheck ./...` locally and read the call stacks. Only findings with a call stack can be reached; report the rest without chasing them. `govulncheck -show verbose ./...` lists the module-level findings that can't be reached, including which platforms each affects.
2. For a finding in a module, bump that module to the fixed version named in the finding with `go get <module>@<fixed>`, then run `go mod tidy`. Never add a module, and never replace one with a fork.
3. For a finding in the standard library, the fix is a newer Go patch release. First check how CI installs Go:
   - With `go-version-file: go.mod`, CI installs exactly what `go.mod` names. Add the `toolchain go1.X.Y` line to `go.mod`, or raise it, to the fixed release. If your local Go is already newer, it hides these findings, but CI will not.
   - With `go-version: stable`, CI uses the latest Go release, so a current local Go reproduces CI's standard-library results. Raising `toolchain` only matters for people building from source.
4. Build and run the full test suite. Then run `govulncheck ./...` again and confirm it exits 0. Quote the output from before and after in the report.
