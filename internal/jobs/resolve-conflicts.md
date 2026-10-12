# Resolve merge conflicts on your own pull request

MARSHAL opened the pull request described below from an earlier run, and
the base branch has since moved so that the two no longer merge. MARSHAL
has started merging the base into this branch and stopped before
committing. You are on that branch now, in the middle of the merge.

1. Do not run `git merge --abort`, `git reset`, `git checkout --` or
   `git commit`. MARSHAL refuses a run whose merge is no longer in
   progress, and it records the merge commit itself.
2. For each file in conflict, read both sides and the base's history for
   it (`git log -p MERGE_HEAD -- <file>`) before editing. Keep what the
   base changed and what this pull request is for. Where the two cannot
   both stand, the base wins and the pull request adapts to it.
3. Remove every conflict marker. A file with `<<<<<<<` or `>>>>>>>` left
   in it fails the run.
4. Change nothing outside what the merge needs. Code the base renamed or
   moved may need updating where this branch used it; that is part of the
   merge. Unrelated fixes are not.
5. Build and run the full test suite on the merged tree before you finish.

Your final message is posted as a comment on the pull request. Its first
line is a short title. Then list each conflicted file and how you resolved
it in one line. If a conflict needs a decision only the operator can make,
resolve nothing in that file, leave its markers, and say what the decision
is; the run stops and a human takes it from there.
