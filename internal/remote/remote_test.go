package remote

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDeploySteps(t *testing.T) {
	d := Deploy{Host: "hermes1", Source: "/src", Fleet: "/c/fleet.yaml", Key: "/c/marshal-app.pem", DB: "/s/marshal.db", Unit: "/src/packaging/marshal.service", Env: []string{"ANTHROPIC_API_KEY=x"}}
	steps := strings.Join(d.Steps(), "\n")
	for _, want := range []string{"GOOS=linux GOARCH=amd64", "scp /c/fleet.yaml hermes1:~/.config/marshal/fleet.yaml", "marshal-app.pem  (mode 600)", "marshal.db", "env on hermes1 (1 line(s)", "enable-linger", "enable --now marshal"} {
		if !strings.Contains(steps, want) {
			t.Errorf("steps missing %q", want)
		}
	}
	var out bytes.Buffer
	d.DryRun = true
	d.Out = &out
	if err := d.Run(context.Background()); err != nil || !strings.Contains(out.String(), "systemctl --user") {
		t.Errorf("dry run: %v\n%s", err, out.String())
	}
	if steps := (Deploy{Host: "h", Source: "/s"}).Steps(); strings.Contains(strings.Join(steps, "\n"), "fleet.yaml") {
		t.Error("nothing optional should appear when not given")
	}
}

func TestOpenWithoutHost(t *testing.T) {
	t.Setenv(HostEnv, "")
	if _, err := Open(context.Background()); err == nil {
		t.Error("no host must fail")
	}
}
