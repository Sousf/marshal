# Fix failing checks on your own pull request

MARSHAL opened the pull request described below from an earlier run, and
its continuous integration checks failed. You are on that branch now. Make
the checks pass without changing what the pull request is for.

1. Read the failing log excerpts below first, then reproduce locally with
   the same commands the workflow runs. A log alone can mislead; a local
   run tells you whether it is the change, the environment, or flakiness.
2. Fix the cause. If the failure is in the change this branch made, fix the
   change. If the failure is a pre-existing problem the change merely
   exposed, fix that only if it is small and clearly related; otherwise stop
   and explain.
3. Do not widen the scope. No new dependencies, no unrelated cleanups, no
   weakening of tests or checks to make them pass. Skipping, deleting or
   loosening a test is not a fix unless the report says exactly why the
   test was wrong.
4. Run the full test suite and the workflow's other steps before you finish.

Your final message is used as a comment on the pull request. State what
failed, why, and what you changed. If you could not fix it, say what is
needed instead; a human will take it from there.
