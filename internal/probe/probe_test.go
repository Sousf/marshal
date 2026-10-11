package probe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sousf/marshal/internal/contract"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOnboardAndPosture(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r := Run(ctx, "onboard", Env{Dir: dir})
	if !r.Needed || !strings.Contains(r.Reason, "AGENTS.md") || !strings.Contains(r.Reason, "pull requests") {
		t.Errorf("empty repo: %+v", r)
	}
	write(t, dir, ".github/workflows/ci.yml", "name: ci\non:\n  pull_request:\npermissions:\n  contents: read\njobs:\n  test:\n    steps:\n      - uses: actions/checkout@v4\n      - uses: ./local\n")
	write(t, dir, ".github/dependabot.yml", "version: 2\n")
	write(t, dir, "AGENTS.md", "# x\n")
	write(t, dir, "marshal.yaml", "contract: 1\n")
	if r := Run(ctx, "onboard", Env{Dir: dir}); r.Needed {
		t.Errorf("complete repo should not need onboarding: %+v", r)
	}
	// Posture: one unpinned action.
	r = Run(ctx, "posture-audit", Env{Dir: dir})
	if !r.Needed || !strings.Contains(r.Reason, "1 action(s) not pinned") || strings.Contains(r.Reason, "permissions") {
		t.Errorf("posture: %+v", r)
	}
	write(t, dir, ".github/workflows/ci.yml", "name: ci\non: [pull_request]\npermissions: {contents: read}\njobs:\n  t:\n    steps:\n      - uses: actions/checkout@abcdef0123456789abcdef0123456789abcdef01 # v4\n")
	if r := Run(ctx, "posture-audit", Env{Dir: dir}); r.Needed {
		t.Errorf("pinned and set: %+v", r)
	}
	write(t, dir, ".github/workflows/release.yml", "name: r\non: push\njobs: {}\n")
	if r := Run(ctx, "posture-audit", Env{Dir: dir}); !r.Needed || !strings.Contains(r.Reason, "release.yml: no top-level permissions") {
		t.Errorf("missing permissions: %+v", r)
	}
}

func TestVulnWithContract(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clean, _ := contract.Parse([]byte("contract: 1\nverbs: {vuln: 'true'}\n"))
	if r := Run(ctx, "vuln-fix", Env{Dir: dir, Contract: clean}); r.Needed {
		t.Errorf("clean scanner: %+v", r)
	}
	red, _ := contract.Parse([]byte("contract: 1\nverbs: {vuln: 'echo GO-2026-1 found; false'}\n"))
	if r := Run(ctx, "vuln-fix", Env{Dir: dir, Contract: red}); !r.Needed || !strings.Contains(r.Reason, "GO-2026-1") {
		t.Errorf("red scanner: %+v", r)
	}
	gone, _ := contract.Parse([]byte("contract: 1\nverbs: {vuln: 'no-such-scanner-xyz'}\n"))
	if r := Run(ctx, "vuln-fix", Env{Dir: dir, Contract: gone}); r.Needed {
		t.Errorf("missing scanner is not work: %+v", r)
	}
	if r := Run(ctx, "vuln-fix", Env{Dir: dir}); r.Needed {
		t.Errorf("no go.mod, no contract: nothing to scan: %+v", r)
	}
}

func TestReflectAndUnknown(t *testing.T) {
	ctx := context.Background()
	if r := Run(ctx, "custom-job", Env{}); !r.Needed {
		t.Error("jobs without a probe always run")
	}
	if r := Run(ctx, "reflect", Env{}); !r.Needed {
		t.Error("unknown feedback state runs")
	}
	last := time.Now().Add(-time.Hour)
	yes := func(t time.Time) bool { return t.Equal(last) }
	if r := Run(ctx, "reflect", Env{LastRun: last, FeedbackSince: yes}); !r.Needed {
		t.Error("new feedback needs reflection")
	}
	no := func(time.Time) bool { return false }
	if r := Run(ctx, "reflect", Env{LastRun: last, FeedbackSince: no}); r.Needed {
		t.Error("no feedback, no reflection")
	}
}
