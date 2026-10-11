// Package jobs embeds the default briefs. A fleet file can override any of
// them with prompt or prompt_file; these are the ones MARSHAL ships with.
package jobs

import (
	"embed"
	"sort"
	"strings"
)

//go:embed *.md
var files embed.FS

// Prompt returns the embedded brief for a job name.
func Prompt(name string) (string, bool) {
	b, err := files.ReadFile(name + ".md")
	if err != nil {
		return "", false
	}
	return string(b), true
}

// Names lists the embedded briefs.
func Names() []string {
	entries, _ := files.ReadDir(".")
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".md"))
	}
	sort.Strings(out)
	return out
}

// Contract is appended to every brief. It is the part of the prompt the
// operator should not have to repeat per job, and the part the harness
// relies on: the final message becomes the PR, and git is not the agent's.
const Contract = `
## How MARSHAL runs you

- You are in a fresh clone of the repository on a branch of your own. Edit files in place.
- Do not commit, push, open pull requests, or touch any git remote. MARSHAL commits what you changed after checking it against policy.
- If the repository has an AGENTS.md, its conventions outrank this brief.
- Run the project's tests before you finish. A change that does not pass them is not finished.
- Stay inside the task. Fix nothing you were not asked to fix; note it in your report instead.
- Your final message is used verbatim as the pull request. Format it as:
  - First line: a commit-style title under 70 characters.
  - Blank line, then the body: what changed, why, how you verified it, and anything the reviewer should look at.
  - If you changed nothing, say so in one line starting with "NO CHANGE:" and explain why.
`
