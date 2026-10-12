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

func TestParseHome(t *testing.T) {
	for _, c := range []struct {
		out, want string
		ok        bool
	}{
		{"MARSHAL_HOME=/home/pete\n", "/home/pete", true},
		{"welcome to hermes1\r\nMARSHAL_HOME=/home/pete\r\n", "/home/pete", true},
		{"MARSHAL_HOME=/\n", "/", true},
		{"MARSHAL_HOME=\n", "", false},
		{"MARSHAL_HOME=relative\n", "", false},
		{"MARSHAL_HOME=/a:b\n", "", false},
		{"MARSHAL_HOME=/a\x1bb\n", "", false},
		{"/home/pete\n", "", false},
	} {
		got, err := parseHome(c.out)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("parseHome(%q) = %q, %v", c.out, got, err)
		}
	}
}

func TestOpenRejectsOptionHost(t *testing.T) {
	t.Setenv(HostEnv, "-oProxyCommand=true")
	if _, err := Open(context.Background()); err == nil {
		t.Error("a host starting with a dash must be refused")
	}
}

func TestDeployBrewSteps(t *testing.T) {
	d := Deploy{Host: "hermes1", Source: "/src", Unit: "/src/packaging/marshal.service", Brew: true,
		BrewPath: "/home/linuxbrew/.linuxbrew/bin/brew", Formula: "sousf/tap/marshal"}
	steps := strings.Join(d.Steps(), "\n")
	for _, want := range []string{"brew install --formula sousf/tap/marshal", "marshal-upgrade.timer", "/home/linuxbrew/.linuxbrew/opt/marshal/bin/marshal"} {
		if !strings.Contains(steps, want) {
			t.Errorf("brew steps missing %q:\n%s", want, steps)
		}
	}
	if strings.Contains(steps, "go build") {
		t.Error("a brew deploy installs the released binary; it builds nothing")
	}
	plain := strings.Join(Deploy{Host: "h", Source: "/s"}.Steps(), "\n")
	if !strings.Contains(plain, "disable --now marshal-upgrade.timer") {
		t.Error("a binary deploy must stop brew upgrades replacing what it installed")
	}
}

func TestBrewUnits(t *testing.T) {
	unit := []byte("[Service]\nExecStart=%h/.local/bin/marshal serve\nRestart=on-failure\n")
	svc, up, timer, err := brewUnits(unit, "/home/linuxbrew/.linuxbrew/bin/brew", "sousf/tap/marshal")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(svc, "ExecStart=/home/linuxbrew/.linuxbrew/opt/marshal/bin/marshal serve\n") || strings.Contains(svc, ".local/bin/marshal") {
		t.Errorf("service:\n%s", svc)
	}
	if !strings.Contains(up, "ExecStart=/home/linuxbrew/.linuxbrew/bin/brew upgrade --formula sousf/tap/marshal") || !strings.Contains(timer, "OnUnitActiveSec=") {
		t.Errorf("upgrade:\n%s\ntimer:\n%s", up, timer)
	}
	for _, bad := range [][2]string{{"brew", "sousf/tap/marshal"}, {"/b/brew", "x; rm -rf ~"}, {"/b/brew $(id)", "a/b/c"}} {
		if _, _, _, err := brewUnits(unit, bad[0], bad[1]); err == nil {
			t.Errorf("should refuse brew=%q formula=%q", bad[0], bad[1])
		}
	}
}
