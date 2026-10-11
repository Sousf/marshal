---
name: Go dead-code and inefficiency scan
description: Find and remove unused code and wasteful idioms in a Go module using analyzers instead of reading everything by hand.
learned_from: system-patch/f80e4666
---

1. Run `go run golang.org/x/tools/cmd/deadcode@latest -test ./...` and `go run golang.org/x/tools/cmd/deadcode@latest ./...`. Anything listed by the first command is dead everywhere. Anything listed only by the second is used only by tests: read its doc comment before removing it, because some test-only helpers are deliberate.
2. Run `go run mvdan.cc/unparam@latest ./...` for unused parameters and results.
3. Run `go run golang.org/x/tools/go/analysis/passes/modernize/cmd/modernize@latest ./...` to list everything. Apply only the categories that are about efficiency, one at a time with its flag, e.g. `... -stringsseq -fix ./...`. Leave style-only categories alone unless asked.
4. If `-fix` touched `_test.go` files you don't want in the diff, revert those hunks by hand (`git checkout` may need approval).
5. Then look for free optimisations the analyzers can't see. If you can start subagents, give each one dimension with read-only tools: allocations and copies, repeated subprocess/file/network calls (start with hot paths: UI render/View functions, per-request handlers), algorithmic cost, locking and concurrency, startup time. Otherwise do the passes yourself. Free means same behaviour and output, no new dependency, no harder-to-read code. Fix every free one in this same change, so it ships in the PR MARSHAL opens. Anything that is a trade-off goes in the report, not the diff.
6. Run `gofmt -l .`, vet, build, tests and govulncheck, then run `deadcode` again to confirm the only remaining entries are intentional.
