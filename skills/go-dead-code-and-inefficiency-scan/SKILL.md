---
name: Go dead-code and inefficiency scan
description: Find and remove unused code and wasteful idioms in a Go module using analyzers instead of reading everything by hand.
learned_from: system-patch/f80e4666
---

1. Run `go run golang.org/x/tools/cmd/deadcode@latest -test ./...` and `go run golang.org/x/tools/cmd/deadcode@latest ./...`. Anything listed by the first command is dead everywhere. Anything listed only by the second is used only by tests: read its doc comment before removing it, because some test-only helpers are deliberate.
2. Run `go run mvdan.cc/unparam@latest ./...` for unused parameters and results.
3. Run `go run golang.org/x/tools/go/analysis/passes/modernize/cmd/modernize@latest ./...` to list everything. Apply only the categories that are about efficiency, one at a time with its flag, e.g. `... -stringsseq -fix ./...`. Leave style-only categories alone unless asked.
4. If `-fix` touched `_test.go` files you don't want in the diff, revert those hunks by hand (`git checkout` may need approval).
5. Check hot paths by hand (UI render/View functions, per-request handlers) for uncached subprocess, file or network calls, which analyzers don't catch.
6. Run `gofmt -l .`, vet, build, tests and govulncheck, then run `deadcode` again to confirm the only remaining entries are intentional.
