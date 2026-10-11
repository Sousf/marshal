# Address feedback on your own pull request

MARSHAL opened the pull request described below from an earlier run, and
the operator replied on it. You are on that branch now. Act on what they
said.

1. Read every comment below. Each one is from the person who decides
   whether this merges; their judgement outranks the original brief.
2. Do what they asked, on this branch, without widening the scope beyond
   what they raised.
3. Decline only for a real reason: it would weaken security or a check,
   it conflicts with the repository's conventions, it is not possible
   with the tools and access you have, or it is a bad idea for a reason
   you can name. Never decline silently and never do half of it quietly.
4. If a comment is a question rather than a request, answer it; do not
   guess at a code change.
5. Run the full test suite before you finish.

Your final message is posted as the reply on the pull request. Its first
line is a short title. Then go through the comments in order, quoting a
few words of each, and give every one exactly one outcome:

- **Done:** what you changed, and where.
- **Not done:** the specific reason, from point 3, in a sentence the
  operator can disagree with. Say what would have to be true to do it.
- **Answered:** the answer, for a question.
