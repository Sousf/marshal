package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fake gives New a bwrap to find without needing bubblewrap installed.
func fake(t *testing.T, opt Options) *Sandbox {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bwrap")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	opt.Bwrap = bin
	s, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNilSandboxLeavesCommandAlone(t *testing.T) {
	cmd := exec.Command("sh", "-c", "true")
	before := append([]string{}, cmd.Args...)
	var s *Sandbox
	s.Wrap(cmd, nil, nil)
	if strings.Join(cmd.Args, " ") != strings.Join(before, " ") {
		t.Fatalf("args changed: %v", cmd.Args)
	}
	if s.Probe() != nil || s.String() != "none" {
		t.Fatal("nil sandbox should be a no-op")
	}
}

func TestWrapHidesThenBindsTheRunDir(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "state", "work", "run-1")
	if err := os.MkdirAll(filepath.Join(work, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := fake(t, Options{Hide: []string{home}, Mask: []string{key}})
	cmd := exec.Command("sh", "-c", "go test ./...")
	cmd.Dir = work
	s.Wrap(cmd, nil, nil)
	if cmd.Err != nil {
		t.Fatal(cmd.Err)
	}
	got := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"--ro-bind / /",
		"--tmpfs /tmp",
		"--tmpfs " + home,
		"--ro-bind " + os.DevNull + " " + key,
		"--bind " + work + " " + work,
		"--ro-bind " + work + "/.git " + work + "/.git",
		"--chdir " + work,
		"--unshare-all",
		"--new-session",
		"--die-with-parent",
		"-- " + cmd.Args[len(cmd.Args)-3] + " -c go test ./...",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	// Order matters: a bind before the tmpfs over its parent would vanish,
	// and .git must be remounted read-only after the writable tree.
	if strings.Index(got, "--tmpfs "+home) > strings.Index(got, "--bind "+work) {
		t.Error("run dir bound before the home directory was hidden")
	}
	if strings.Index(got, "--bind "+work) > strings.Index(got, "--ro-bind "+work+"/.git") {
		t.Error(".git made read-only before the work tree was bound")
	}
	if cmd.Path != s.bwrap {
		t.Errorf("path = %s, want bwrap", cmd.Path)
	}
}

func TestBindsMayNotExposeHiddenPaths(t *testing.T) {
	home := t.TempDir()
	conf := filepath.Join(home, ".config", "marshal")
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Bwrap: "sh", Hide: []string{conf}, ReadOnly: []string{filepath.Join(home, ".config")}}); err == nil {
		t.Error("a read-only bind over a hidden directory should be refused")
	}
	if _, err := New(Options{Bwrap: "sh", Hide: []string{"/"}}); err == nil {
		t.Error("hiding / should be refused")
	}
	if _, err := New(Options{Bwrap: "sh", Hide: []string{"relative"}}); err == nil {
		t.Error("a relative path should be refused")
	}
	s := fake(t, Options{Hide: []string{conf}})
	cmd := exec.Command("true")
	cmd.Dir = home
	s.Wrap(cmd, nil, nil)
	if cmd.Err == nil {
		t.Error("a run dir that contains a hidden directory should fail to start")
	}
}

func TestHostOptionsKeepsPathEntriesThatDoNotExposeAnything(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, ".local", "share", "marshal")
	bin := filepath.Join(home, ".local", "bin")
	t.Setenv("PATH", strings.Join([]string{bin, home, filepath.Join(home, ".local"), "/usr/bin", "rel"}, string(os.PathListSeparator)))
	opt := HostOptions([]string{home, state}, nil, nil, nil)
	if len(opt.ReadOnly) == 0 || opt.ReadOnly[0] != bin {
		t.Fatalf("want %s first, got %v", bin, opt.ReadOnly)
	}
	for _, p := range opt.ReadOnly {
		if p == home || p == filepath.Join(home, ".local") || p == "/usr/bin" || p == "rel" {
			t.Errorf("should not bind %s", p)
		}
	}
}

// TestLive runs a command under real bubblewrap when the host has a working
// one, and checks what it can and cannot touch.
func TestLive(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap not installed")
	}
	root := t.TempDir()
	secret := filepath.Join(root, "home", ".config", "marshal")
	work := filepath.Join(root, "home", "state", "work", "run-1")
	for _, d := range []string{secret, filepath.Join(work, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(root, "app.pem")
	for _, f := range []string{filepath.Join(secret, "fleet.yaml"), key} {
		if err := os.WriteFile(f, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(Options{Hide: []string{filepath.Join(root, "home")}, Mask: []string{key}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Probe(); err != nil {
		t.Skip(err)
	}
	run := func(script string) (string, error) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = work
		s.Wrap(cmd, nil, nil)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("cat " + filepath.Join(secret, "fleet.yaml")); err == nil {
		t.Errorf("hidden file readable: %q", out)
	}
	if out, _ := run("cat " + key); strings.Contains(out, "secret") {
		t.Errorf("masked key readable: %q", out)
	}
	if out, err := run("echo ok > made && cat made"); err != nil || strings.TrimSpace(out) != "ok" {
		t.Errorf("run dir not writable: %v %q", err, out)
	}
	if _, err := run("echo x > .git/hooks-planted"); err == nil {
		t.Error(".git writable inside the sandbox")
	}
}
