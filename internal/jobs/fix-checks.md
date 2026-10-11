# Fix failing checks on your own pull request

MARSHAL opened the pull request described below from an earlier run, and
its continuous integration checks failed. You are on that branch now. Make
the checks pass without changing what the pull request is for.

1. Read the failing log excerpts below first, then reproduce locally with
   the same commands the workflow runs. A log alone can mislead; a local
   run tells you whether it is the change, the environment, or flakiness.
2. Fix the cause, and finish the job. If the failure is in the change this
   branch made, fix the change. If the branch added a check and the check
   found real problems, those problems are now this pull request's to fix:
   a vulnerability scanner finding is fixed by bumping the affected module
   to the fixed version, a standard-library finding by adding a
   `toolchain` line in `go.mod` for the fixed Go release, a vet or lint
   finding by correcting the code. Make every check this branch introduced
   pass before you finish. Do not leave a known fix "for the maintainer";
   you are the maintainer, and a human reviews the result.
3. Do not widen the scope beyond what the checks require. No new
   dependencies, no unrelated cleanups, no weakening of tests or checks to
   make them pass. Skipping, deleting or loosening a test is not a fix
   unless the report says exactly why the test was wrong. Stop and explain
   only when the fix would change the project's behaviour in a way a user
   could notice, or would need a decision the code cannot tell you.
4. Run the full test suite and the workflow's other steps before you finish.

Your final message is used as a comment on the pull request. State what
failed, why, and what you changed. If you could not fix it, say what is
needed instead; a human will take it from there.
