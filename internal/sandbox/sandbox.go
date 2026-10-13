// Package sandbox runs untrusted commands (the agent, a repository's contract
// verbs) under bubblewrap, so code in a repository cannot read what the
// daemon's user can: the GitHub App key, the fleet file, the database and
// socket, ssh keys, a gh login.
//
// The view inside is the host's root read-only, with the daemon user's home,
// the MARSHAL config and state directories and the runtime directory replaced
// by empty tmpfs mounts. The run's own clone is the only writable host path
// besides the caches the operator shares on purpose, and its .git is
// read-only so nothing in it can plant a hook or config that the harness's
// own git would later execute outside the sandbox.
//
// Network is not isolated yet: see SECURITY.md.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Options describe one host's sandbox. Paths must be absolute.
type Options struct {
	// Bwrap is the bubblewrap binary; empty means "bwrap" on PATH.
	Bwrap string
	// Hide are directories replaced with an empty tmpfs.
	Hide []string
	// Mask are files replaced with an empty one, for secrets that live
	// outside every hidden directory.
	Mask []string
	// ReadOnly are host paths under a hidden directory that runs still
	// need to read, such as a toolchain installed in the home directory.
	ReadOnly []string
	// Writable are host paths shared read-write between runs, such as the
	// Go build and module caches. Anything a run writes there is seen by
	// later runs, so list only caches.
	Writable []string
}

// Sandbox wraps commands in bubblewrap. A nil *Sandbox runs them unwrapped.
type Sandbox struct {
	bwrap string
	opt   Options
}

// New checks the options and that bubblewrap is installed. It does not prove
// bubblewrap works on this kernel; Probe does.
func New(opt Options) (*Sandbox, error) {
	bin := opt.Bwrap
	if bin == "" {
		bin = "bwrap"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("isolation: bubblewrap not found (%w); install the bubblewrap package or set isolation.mode to none", err)
	}
	for _, list := range [][]string{opt.Hide, opt.Mask, opt.ReadOnly, opt.Writable} {
		for _, p := range list {
			if !filepath.IsAbs(p) {
				return nil, fmt.Errorf("isolation: %q is not an absolute path", p)
			}
		}
	}
	for _, h := range opt.Hide {
		if filepath.Clean(h) == "/" {
			return nil, errors.New("isolation: refusing to hide /")
		}
	}
	s := &Sandbox{bwrap: path, opt: opt}
	if err := s.exposes(append(append([]string{}, opt.ReadOnly...), opt.Writable...)); err != nil {
		return nil, err
	}
	return s, nil
}

// exposes refuses a bind that would put a hidden directory or masked file
// back in view: binds are mounted after the tmpfs and would cover it.
func (s *Sandbox) exposes(paths []string) error {
	secret := append(append([]string{}, s.opt.Hide...), s.opt.Mask...)
	for _, p := range paths {
		if p != "" && covers(p, secret) {
			return fmt.Errorf("isolation: binding %s would expose a hidden path", p)
		}
	}
	return nil
}

// Probe runs a trivial command in the sandbox. Unprivileged user namespaces
// can be disabled by the kernel or AppArmor, and then every run would fail
// one at a time; better to fail once at startup.
func (s *Sandbox) Probe() error {
	if s == nil {
		return nil
	}
	cmd := exec.Command("true")
	cmd.Dir = os.TempDir()
	s.Wrap(cmd, nil, nil)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("isolation: bubblewrap does not work here: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Args returns the bubblewrap arguments for a command run in dir, before
// the "--" that separates them from the command. Exposed for tests.
func (s *Sandbox) Args(dir string, readOnly, writable []string) []string {
	a := []string{
		"--die-with-parent",
		// A new session, so the command cannot push input into a
		// terminal the daemon happens to hold (CVE-2017-5226).
		"--new-session",
		"--unshare-all",
		// No egress proxy yet; see SECURITY.md.
		"--share-net",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}
	for _, h := range s.opt.Hide {
		a = append(a, "--tmpfs", h)
	}
	for _, m := range s.opt.Mask {
		if exists(m) {
			a = append(a, "--ro-bind", os.DevNull, m)
		}
	}
	// Binds come after the tmpfs mounts, which they punch through.
	for _, p := range append(append([]string{}, s.opt.ReadOnly...), readOnly...) {
		if exists(p) {
			a = append(a, "--ro-bind", p, p)
		}
	}
	for _, p := range append(append([]string{}, s.opt.Writable...), writable...) {
		if exists(p) {
			a = append(a, "--bind", p, p)
		}
	}
	if dir != "" {
		a = append(a, "--bind", dir, dir)
		if g := filepath.Join(dir, ".git"); exists(g) {
			a = append(a, "--ro-bind", g, g)
		}
		a = append(a, "--chdir", dir)
	}
	return a
}

// Wrap rewrites cmd to run inside the sandbox, with cmd.Dir as its only
// writable working tree plus the extra paths given. A nil Sandbox leaves cmd
// alone. Call it after cmd.Dir is set and before Start.
func (s *Sandbox) Wrap(cmd *exec.Cmd, readOnly, writable []string) {
	if s == nil || cmd.Err != nil {
		return
	}
	dir := cmd.Dir
	if dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
	}
	if err := s.exposes(append(append([]string{dir}, readOnly...), writable...)); err != nil {
		cmd.Err = err
		return
	}
	args := append([]string{s.bwrap}, s.Args(dir, readOnly, writable)...)
	args = append(append(args, "--", cmd.Path), cmd.Args[1:]...)
	cmd.Path = s.bwrap
	cmd.Args = args
}

// String describes the sandbox for `marshal check`.
func (s *Sandbox) String() string {
	if s == nil {
		return "none"
	}
	return fmt.Sprintf("bubblewrap (%s), hiding %s", s.bwrap, strings.Join(s.opt.Hide, ", "))
}

var current atomic.Pointer[Sandbox]

// Use sets the process-wide sandbox that Current returns. Set once at
// startup, so the runner and the contract cannot each be handed a different
// one, or none, by a caller that forgot.
func Use(s *Sandbox) { current.Store(s) }

// Current is the process-wide sandbox, nil when isolation is off.
func Current() *Sandbox { return current.Load() }

// UnderAny reports whether p is one of roots or lies beneath one.
func UnderAny(p string, roots []string) bool {
	for _, r := range roots {
		if rel, err := filepath.Rel(r, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return true
		}
	}
	return false
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
