# Vulnerability fix

Run the project's vulnerability scanner and fix what it reports.

For Go: `govulncheck ./...`. For other ecosystems use the native tool
(`npm audit`, `pip-audit`, `cargo audit`).

For each finding:

1. Confirm it is reachable, not just present in the module graph. The
   scanner's call-stack output tells you. Unreachable findings are reported,
   not fixed.
2. Bump the affected module to the fixed version with the smallest possible
   jump. Do not add new dependencies and do not replace a module with a fork.
3. If the fix is a major version with breaking changes, make the code
   changes needed and explain them in the report. If the breakage is large,
   stop and describe what it would take instead of half-doing it.
4. Build and run the full test suite.

Finish with a clean scanner run, or an exact explanation of what remains and
why it cannot be fixed yet.
