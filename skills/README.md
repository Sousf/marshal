# MARSHAL memory

Procedures MARSHAL learned on earlier runs, and `OPERATOR.md`, its notes on
the person it works for. Every run on every repository reads this
directory. Nothing enters it except through a pull request to this repo,
and the `memory` class never merges on its own.

A skill is a directory with a `SKILL.md`: YAML frontmatter with `name`,
`description`, optional `tags` and `learned_from`, then the procedure.
Proposals come from runs that end their report with `## Skill: <name>`.
Content that pipes downloads to a shell, handles credentials, or weakens a
check is refused before a pull request is opened.

`write-docs` and `i-have-adhd` are the operator's own skills, copied in
from their Claude Code setup and kept separate from it from then on. A
skill longer than a page cannot be replaced from a run's report; runs that
find one wrong leave a correction, and the daily `reflect` job edits the
skill in place. Every change still arrives as a pull request.
