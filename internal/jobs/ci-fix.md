# CI fix

The default branch's continuous integration is failing. Find out why and fix
it.

1. Read the most recent failed run: `gh run list --branch <default> --limit 5`
   then `gh run view <id> --log-failed`.
2. Reproduce locally where possible. Do not trust the log alone; a flaky
   test and a real break look identical in one run.
3. Fix the cause, not the symptom. Skipping a test, loosening an assertion,
   or pinning to an older toolchain are not fixes unless the report explains
   why they are the right call here.
4. Run the full test suite and any linters the workflow runs.

Quote the failing log lines in your report so the reviewer can match them to
the change.
