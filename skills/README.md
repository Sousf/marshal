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
