// Package chat opens Claude Code as MARSHAL's conversation window: the full
// interactive interface, with MARSHAL's context in the system prompt and
// its tool surface reduced to the marshal command line, which enforces
// every rule the daemon does. Nothing new to render, nothing new to trust.
package chat

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sousf/marshal/internal/api"
	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/store"
)

// sessionKey is the settings row holding the conversation to resume.
const sessionKey = "chat_session"

// Allowed is the chat's whole tool surface. Actions go through marshal,
// which talks to the daemon over the socket and applies policy; reading and
// searching the web are harmless; files are read-only.
var Allowed = []string{
	"Bash(marshal:*)", "Read", "Grep", "Glob", "WebSearch", "WebFetch",
}

// Disallowed closes the doors the operator should not open from a chat,
// even by accident.
var Disallowed = []string{
	"Edit", "Write", "NotebookEdit", "Task",
	"Bash(git:*)", "Bash(gh:*)", "Bash(curl:*)", "Bash(wget:*)", "Bash(sudo:*)", "Bash(rm:*)", "Bash(ssh:*)",
}

// Options for one chat.
type Options struct {
	// New starts a fresh conversation instead of resuming the last one.
	New bool
	// Binary overrides "claude" on PATH.
	Binary string
}

// Command builds the Claude Code process. The caller runs it attached to
// the terminal. The working directory is an empty, dedicated one so the
// file tools have nothing to wander into.
func Command(ctx context.Context, f *config.Fleet, st *store.Store, status *api.Status, recent []store.Run, opts Options) (*exec.Cmd, error) {
	dir := filepath.Join(config.StateDir(), "chat")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	bin := opts.Binary
	if bin == "" {
		bin = "claude"
	}
	args := []string{
		"--append-system-prompt", SystemPrompt(f, status, recent),
		"--allowedTools",
	}
	args = append(args, Allowed...)
	args = append(args, "--disallowedTools")
	args = append(args, Disallowed...)

	session, _ := st.Setting(ctx, sessionKey, "")
	if opts.New || session == "" {
		session = newUUID()
		args = append(args, "--session-id", session)
		_ = st.SetSetting(ctx, sessionKey, session)
	} else {
		args = append(args, "--resume", session)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	return cmd, nil
}

// Forget drops the stored conversation so the next chat starts fresh.
func Forget(ctx context.Context, st *store.Store) error {
	return st.SetSetting(ctx, sessionKey, "")
}

// SystemPrompt is what the chat knows about MARSHAL and how it may act.
func SystemPrompt(f *config.Fleet, status *api.Status, recent []store.Run) string {
	var b strings.Builder
	b.WriteString(`You are MARSHAL's chat: the operator's window onto an autonomous repository maintainer. The operator is present and in charge. Be brief and concrete; this is a terminal.

## What MARSHAL is

MARSHAL runs jobs against a fleet of repositories with a coding agent, gates every change behind policy, opens pull requests as a GitHub App, fixes its own red checks, verifies before any autonomous merge, reverts when a merge turns the default branch red, and learns skills and lessons through reviewed pull requests. A daemon schedules the work; the operator reviews on GitHub and watches here.

## How you act

You act only by running the marshal command line with the Bash tool. It talks to the daemon and enforces every rule; you never work around it. You cannot edit files, use git or gh, or reach the network except through the tools you have. When the operator asks for something MARSHAL can do, do it; when they ask for something it cannot, say so plainly.

Commands:
- marshal status · marshal runs [repo] · marshal show <run-id> · marshal trust · marshal digest
- marshal repos · marshal jobs · marshal prompt <job> <repo>
- marshal trigger <job> <repo>        start a job in the background
- marshal task "<instruction>" [--repo <name>]   free-form work; with a repo it ends in a PR, without it in a report
- marshal fix <run-id>                 push a fix to a proposal's PR now
- marshal verify <run-id>              run the verifier on a proposal
- marshal autonomy on|off              the kill switch
- marshal app status · marshal notify test · marshal logs

Runs cost money and open pull requests the operator must review. Start one when asked; do not start several on your own initiative.

`)
	fmt.Fprintf(&b, "## The fleet right now (%s)\n\n", time.Now().Format("Jan 2 15:04"))
	if status != nil {
		auto := "on"
		if !status.Autonomy {
			auto = "OFF"
		}
		fmt.Fprintf(&b, "Daemon %s · autonomy %s · %d run(s) in flight · $%.2f spent in 24h\n\n", status.Version, auto, len(status.Running), status.Spend24h)
	} else {
		b.WriteString("Daemon: down. Commands that start work will fail until `marshal serve` runs; reading works.\n\n")
	}
	for _, r := range f.Repos {
		fmt.Fprintf(&b, "- %s: jobs %s\n", r.Name, strings.Join(r.Jobs, ", "))
	}
	if len(recent) > 0 {
		b.WriteString("\nRecent runs:\n")
		for i, r := range recent {
			if i >= 8 {
				break
			}
			line := fmt.Sprintf("- %s %s %s %s", r.ID, r.Repo, r.Job, r.Status)
			if r.PRURL != "" {
				line += " " + r.PRURL
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

func newUUID() string {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
