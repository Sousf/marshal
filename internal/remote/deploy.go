package remote

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Deploy is what marshal deploy ships and does. Everything goes over ssh
// and scp as the same user, into the same paths MARSHAL uses locally, so
// the fleet file's absolute key path keeps working.
type Deploy struct {
	Host string
	// Source is the repository to build from.
	Source string
	// Fleet, Key and DB are the local files to copy; empty skips one.
	Fleet string
	Key   string
	DB    string
	// Unit is the systemd user unit to install.
	Unit string
	// Env lines written to ~/.config/marshal/env for the service, such as
	// ANTHROPIC_API_KEY=... Empty means none.
	Env []string
	// Brew installs MARSHAL with the host's brew from Formula instead of
	// copying a binary built here, and adds a timer that keeps it upgraded.
	Brew     bool
	BrewPath string
	Formula  string
	// DryRun prints the plan instead of running it.
	DryRun bool
	Out    io.Writer
}

// Steps renders the plan as the shell commands it will run.
func (d Deploy) Steps() []string {
	bin := "~/.local/bin/marshal"
	var steps []string
	if d.Brew {
		steps = []string{
			fmt.Sprintf("ssh %s mkdir -p ~/.local/bin ~/.config/marshal ~/.local/share/marshal ~/.config/systemd/user", d.Host),
			fmt.Sprintf("ssh %s %s update && %s install --formula %s (or upgrade)", d.Host, d.BrewPath, filepath.Base(d.BrewPath), d.Formula),
			fmt.Sprintf("ssh %s ln -s %s %s", d.Host, filepath.Join(brewPrefix(d.BrewPath), "bin", "marshal"), bin),
			fmt.Sprintf("scp marshal.service %s:~/.config/systemd/user/  (ExecStart=%s serve)", d.Host, brewExec(d.BrewPath)),
			fmt.Sprintf("scp marshal-upgrade.service marshal-upgrade.timer %s:~/.config/systemd/user/  (brew upgrade hourly, outside the daemon's sandbox)", d.Host),
		}
	} else {
		steps = []string{
			fmt.Sprintf("local: GOOS=linux GOARCH=amd64 go build -o <tmp>/marshal %s", d.Source),
			fmt.Sprintf("ssh %s mkdir -p ~/.local/bin ~/.config/marshal ~/.local/share/marshal ~/.config/systemd/user", d.Host),
			fmt.Sprintf("ssh %s systemctl --user disable --now marshal-upgrade.timer  (if a brew deploy left one)", d.Host),
			fmt.Sprintf("scp <tmp>/marshal %s:%s", d.Host, bin),
		}
	}
	if d.Fleet != "" {
		steps = append(steps, fmt.Sprintf("scp %s %s:~/.config/marshal/fleet.yaml", d.Fleet, d.Host))
	}
	if d.Key != "" {
		steps = append(steps, fmt.Sprintf("scp %s %s:~/.config/marshal/%s  (mode 600)", d.Key, d.Host, filepath.Base(d.Key)))
	}
	if d.DB != "" {
		steps = append(steps, fmt.Sprintf("scp %s %s:~/.local/share/marshal/marshal.db  (trust ladder, history)", d.DB, d.Host))
	}
	if len(d.Env) > 0 {
		steps = append(steps, fmt.Sprintf("write ~/.config/marshal/env on %s (%d line(s), mode 600)", d.Host, len(d.Env)))
	}
	if d.Unit != "" && !d.Brew {
		steps = append(steps, fmt.Sprintf("scp %s %s:~/.config/systemd/user/marshal.service", d.Unit, d.Host))
	}
	steps = append(steps,
		fmt.Sprintf("ssh %s %s check", d.Host, bin),
		fmt.Sprintf("ssh %s loginctl enable-linger  (so the service runs while logged out)", d.Host),
		fmt.Sprintf("ssh %s systemctl --user daemon-reload && systemctl --user enable --now marshal%s", d.Host, map[bool]string{true: " marshal-upgrade.timer"}[d.Brew]),
		fmt.Sprintf("ssh %s systemctl --user --no-pager status marshal", d.Host),
	)
	return steps
}

// Run executes the plan.
func (d Deploy) Run(ctx context.Context) error {
	if d.Out == nil {
		d.Out = io.Discard
	}
	if d.DryRun {
		for _, s := range d.Steps() {
			fmt.Fprintln(d.Out, "  "+s)
		}
		return nil
	}
	tmp, err := os.MkdirTemp("", "marshal-deploy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	bin := filepath.Join(tmp, "marshal")
	var units map[string]string
	if d.Brew {
		raw, err := os.ReadFile(d.Unit)
		if err != nil {
			return err
		}
		svc, up, timer, err := brewUnits(raw, d.BrewPath, d.Formula)
		if err != nil {
			return err
		}
		units = map[string]string{"marshal.service": svc, "marshal-upgrade.service": up, "marshal-upgrade.timer": timer}
	} else {
		fmt.Fprintln(d.Out, "building linux/amd64 binary")
		build := exec.CommandContext(ctx, "go", "build", "-ldflags", "-X main.version="+version(d.Source), "-o", bin, ".")
		build.Dir = d.Source
		build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			return fmt.Errorf("build: %w\n%s", err, out)
		}
	}
	ssh := func(args string) error {
		fmt.Fprintln(d.Out, "ssh "+d.Host+" "+args)
		cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", d.Host, args)
		cmd.Stdout, cmd.Stderr = d.Out, d.Out
		return cmd.Run()
	}
	scp := func(local, remote, mode string) error {
		fmt.Fprintf(d.Out, "scp %s %s:%s\n", local, d.Host, remote)
		cmd := exec.CommandContext(ctx, "scp", "-q", "-o", "BatchMode=yes", local, d.Host+":"+remote)
		cmd.Stdout, cmd.Stderr = d.Out, d.Out
		if err := cmd.Run(); err != nil {
			return err
		}
		if mode != "" {
			return ssh("chmod " + mode + " " + remote)
		}
		return nil
	}
	if err := ssh("mkdir -p ~/.local/bin ~/.config/marshal ~/.local/share/marshal ~/.config/systemd/user"); err != nil {
		return err
	}
	// A running service holds the binary open; stop it first so the copy
	// does not fail with "text file busy".
	_ = ssh("systemctl --user stop marshal 2>/dev/null || true")
	if d.Brew {
		f, brew := d.Formula, d.BrewPath
		if err := ssh(fmt.Sprintf("NONINTERACTIVE=1 %s update --quiet && { %s list --formula %s >/dev/null 2>&1 && %s upgrade --formula %s || %s install --formula %s; }", brew, brew, f, brew, f, brew, f)); err != nil {
			return fmt.Errorf("brew on %s: %w", d.Host, err)
		}
		// The link keeps marshal on the PATH of an ssh session; the service
		// starts brew's opt path directly.
		if err := ssh("rm -f ~/.local/bin/marshal && ln -s " + filepath.Join(brewPrefix(brew), "bin", "marshal") + " ~/.local/bin/marshal"); err != nil {
			return err
		}
	} else {
		// A brew deploy left a link into the Cellar here and a timer that
		// upgrades it; copying through the link would write into the keg.
		_ = ssh("systemctl --user disable --now marshal-upgrade.timer 2>/dev/null || true")
		if err := ssh("rm -f ~/.local/bin/marshal"); err != nil {
			return err
		}
		if err := scp(bin, "~/.local/bin/marshal", "755"); err != nil {
			return err
		}
	}
	if d.Fleet != "" {
		if err := scp(d.Fleet, "~/.config/marshal/fleet.yaml", "600"); err != nil {
			return err
		}
	}
	if d.Key != "" {
		if err := scp(d.Key, "~/.config/marshal/"+filepath.Base(d.Key), "600"); err != nil {
			return err
		}
	}
	if d.DB != "" {
		if err := scp(d.DB, "~/.local/share/marshal/marshal.db", "600"); err != nil {
			return err
		}
	}
	if len(d.Env) > 0 {
		envFile := filepath.Join(tmp, "env")
		if err := os.WriteFile(envFile, []byte(strings.Join(d.Env, "\n")+"\n"), 0o600); err != nil {
			return err
		}
		if err := scp(envFile, "~/.config/marshal/env", "600"); err != nil {
			return err
		}
	}
	if d.Brew {
		for _, name := range []string{"marshal.service", "marshal-upgrade.service", "marshal-upgrade.timer"} {
			p := filepath.Join(tmp, name)
			if err := os.WriteFile(p, []byte(units[name]), 0o644); err != nil {
				return err
			}
			if err := scp(p, "~/.config/systemd/user/"+name, "644"); err != nil {
				return err
			}
		}
	} else if d.Unit != "" {
		if err := scp(d.Unit, "~/.config/systemd/user/marshal.service", "644"); err != nil {
			return err
		}
	}
	if err := ssh("PATH=$HOME/.local/bin:$PATH marshal check"); err != nil {
		return fmt.Errorf("the fleet file does not validate on %s: %w", d.Host, err)
	}
	var missing []string
	for _, tool := range []string{"gh", "claude"} {
		if err := ssh("PATH=$HOME/.local/bin:$PATH command -v " + tool + " >/dev/null"); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(d.Out, "warning: %s not found on %s; runs will fail until installed and logged in\n", strings.Join(missing, " and "), d.Host)
	}
	_ = ssh("loginctl enable-linger 2>/dev/null || echo 'enable-linger needs a privileged session; run: sudo loginctl enable-linger $USER'")
	enable := "marshal"
	if d.Brew {
		enable += " marshal-upgrade.timer"
	}
	if err := ssh("systemctl --user daemon-reload && systemctl --user enable --now " + enable); err != nil {
		return err
	}
	return ssh("systemctl --user --no-pager --lines=5 status marshal")
}

var (
	formulaRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._+-]*$`)
	pathRE    = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
)

// brewPrefix is the Homebrew prefix a brew executable belongs to.
func brewPrefix(brew string) string { return filepath.Dir(filepath.Dir(brew)) }

// brewExec is the stable path brew keeps pointing at the installed MARSHAL.
func brewExec(brew string) string {
	return filepath.Join(brewPrefix(brew), "opt", "marshal", "bin", "marshal")
}

// brewUnits renders the units for a brew-managed host: the service started
// from brew's opt path, and a timer that upgrades the formula. The upgrade
// runs in its own unit so the daemon's sandbox keeps the brew prefix
// read-only: code the daemon runs for a pull request cannot replace the
// binary it next restarts into. Both values end up in shell commands and
// unit files, so they are held to plain paths and names.
func brewUnits(unit []byte, brew, formula string) (svc, upgrade, timer string, err error) {
	if !pathRE.MatchString(brew) || filepath.Base(brew) != "brew" {
		return "", "", "", fmt.Errorf("brew path %q: want an absolute path to brew", brew)
	}
	if !formulaRE.MatchString(formula) {
		return "", "", "", fmt.Errorf("formula %q: want owner/tap/name", formula)
	}
	var lines []string
	found := false
	for _, l := range strings.Split(string(unit), "\n") {
		if strings.HasPrefix(l, "ExecStart=") {
			l, found = "ExecStart="+brewExec(brew)+" serve", true
		}
		lines = append(lines, l)
	}
	if !found {
		return "", "", "", fmt.Errorf("unit has no ExecStart line")
	}
	upgrade = `# Upgrades MARSHAL from its Homebrew tap. Installed by marshal deploy --brew.
# It runs outside marshal.service, whose sandbox keeps the brew prefix
# read-only; the daemon sees the new binary and restarts into it when idle.
[Unit]
Description=Upgrade MARSHAL from Homebrew
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
Environment=NONINTERACTIVE=1 HOMEBREW_NO_ENV_HINTS=1
ExecStart=` + brew + ` update --quiet
ExecStart=` + brew + ` upgrade --formula ` + formula + `
`
	timer = `# Hourly: releases are rare and brew update is a network call.
[Unit]
Description=Upgrade MARSHAL from Homebrew hourly

[Timer]
OnBootSec=10min
OnUnitActiveSec=1h
RandomizedDelaySec=5min
Persistent=true

[Install]
WantedBy=timers.target
`
	return strings.Join(lines, "\n"), upgrade, timer, nil
}

func version(src string) string {
	cmd := exec.Command("git", "describe", "--always", "--dirty")
	cmd.Dir = src
	out, err := cmd.Output()
	if err != nil {
		return "dev"
	}
	return string(bytes.TrimSpace(out))
}
