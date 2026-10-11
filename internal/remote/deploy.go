package remote

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	// DryRun prints the plan instead of running it.
	DryRun bool
	Out    io.Writer
}

// Steps renders the plan as the shell commands it will run.
func (d Deploy) Steps() []string {
	bin := "~/.local/bin/marshal"
	steps := []string{
		fmt.Sprintf("local: GOOS=linux GOARCH=amd64 go build -o <tmp>/marshal %s", d.Source),
		fmt.Sprintf("ssh %s mkdir -p ~/.local/bin ~/.config/marshal ~/.local/share/marshal ~/.config/systemd/user", d.Host),
		fmt.Sprintf("scp <tmp>/marshal %s:%s", d.Host, bin),
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
	if d.Unit != "" {
		steps = append(steps, fmt.Sprintf("scp %s %s:~/.config/systemd/user/marshal.service", d.Unit, d.Host))
	}
	steps = append(steps,
		fmt.Sprintf("ssh %s %s check", d.Host, bin),
		fmt.Sprintf("ssh %s loginctl enable-linger  (so the service runs while logged out)", d.Host),
		fmt.Sprintf("ssh %s systemctl --user daemon-reload && systemctl --user enable --now marshal", d.Host),
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
	fmt.Fprintln(d.Out, "building linux/amd64 binary")
	build := exec.CommandContext(ctx, "go", "build", "-ldflags", "-X main.version="+version(d.Source), "-o", bin, ".")
	build.Dir = d.Source
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build: %w\n%s", err, out)
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
	if err := scp(bin, "~/.local/bin/marshal", "755"); err != nil {
		return err
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
	if d.Unit != "" {
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
	if err := ssh("systemctl --user daemon-reload && systemctl --user enable --now marshal"); err != nil {
		return err
	}
	return ssh("systemctl --user --no-pager --lines=5 status marshal")
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
