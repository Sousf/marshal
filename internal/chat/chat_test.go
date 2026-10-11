package chat

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Sousf/marshal/internal/api"
	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/store"
)

func TestCommandAndSession(t *testing.T) {
	t.Setenv("MARSHAL_STATE", t.TempDir())
	f, _ := config.Parse([]byte("version: 1\nrepos: [{name: x, url: u, jobs: [j]}]\njobs: [{name: j, class: c, prompt: p}]\npolicy: {classes: {c: {start: review}}}\n"))
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	ctx := context.Background()
	cmd, err := Command(ctx, f, st, &api.Status{Version: "v", Autonomy: true}, []store.Run{{ID: "abcd1234", Repo: "x", Job: "j", Status: store.Proposed, PRURL: "u"}}, Options{Binary: "/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--session-id ") || strings.Contains(args, "--resume") {
		t.Errorf("first chat should start a session: %s", args)
	}
	uuidRE := regexp.MustCompile(`--session-id ([0-9a-f-]{36})`)
	m := uuidRE.FindStringSubmatch(args)
	if m == nil {
		t.Fatalf("no uuid in %s", args)
	}
	for _, want := range []string{"--allowedTools Bash(marshal:*) Read", "--disallowedTools Edit Write", "Bash(git:*)", "Bash(gh:*)"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q", want)
		}
	}
	if !strings.HasSuffix(cmd.Dir, "/chat") {
		t.Errorf("dir = %s", cmd.Dir)
	}
	sp := SystemPrompt(f, &api.Status{Version: "v", Autonomy: false}, nil)
	for _, want := range []string{"autonomy OFF", "- x: jobs j", "marshal task", "never work around it"} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if !strings.Contains(SystemPrompt(f, nil, nil), "Daemon: down") {
		t.Error("down daemon should be stated")
	}

	// Second chat resumes; --new starts over; Forget clears.
	cmd2, _ := Command(ctx, f, st, nil, nil, Options{Binary: "/bin/true"})
	if !strings.Contains(strings.Join(cmd2.Args, " "), "--resume "+m[1]) {
		t.Error("second chat should resume the same session")
	}
	cmd3, _ := Command(ctx, f, st, nil, nil, Options{Binary: "/bin/true", New: true})
	if a := strings.Join(cmd3.Args, " "); !strings.Contains(a, "--session-id") || strings.Contains(a, m[1]) {
		t.Error("--new should mint a different session")
	}
	Forget(ctx, st)
	if v, _ := st.Setting(ctx, sessionKey, ""); v != "" {
		t.Error("forget should clear")
	}
}
