// Package remote lets every marshal command talk to a daemon on another
// machine. The daemon only ever listens on a Unix socket; remote control is
// an ssh tunnel to that socket, so ssh keys are the authentication and no
// port is ever opened. One tunnel per process, torn down on exit.
package remote

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// HostEnv names the machine the daemon runs on. Empty means local.
const HostEnv = "MARSHAL_HOST"

// RemoteSocketEnv overrides the socket path on that machine.
const RemoteSocketEnv = "MARSHAL_REMOTE_SOCKET"

// DefaultRemoteSocket is where a default install keeps it.
const DefaultRemoteSocket = ".local/share/marshal/marshal.sock"

// Host returns the configured remote host, if any.
func Host() string { return strings.TrimSpace(os.Getenv(HostEnv)) }

// Tunnel is a live ssh forward.
type Tunnel struct {
	Host  string
	Local string
	cmd   *exec.Cmd
}

var (
	mu      sync.Mutex
	current *Tunnel
)

// Open starts the tunnel for the configured host, or returns the one this
// process already has. The local end is a socket in a private temp dir.
func Open(ctx context.Context) (*Tunnel, error) {
	mu.Lock()
	defer mu.Unlock()
	if current != nil {
		return current, nil
	}
	host := Host()
	if host == "" {
		return nil, fmt.Errorf("%s is not set", HostEnv)
	}
	remoteSock := strings.TrimSpace(os.Getenv(RemoteSocketEnv))
	if remoteSock == "" {
		remoteSock = DefaultRemoteSocket
	}
	dir, err := os.MkdirTemp("", "marshal-tunnel-")
	if err != nil {
		return nil, err
	}
	local := filepath.Join(dir, "marshal.sock")
	// -N: no command. ExitOnForwardFailure: a refused forward is an error
	// now, not a hang later. StreamLocalBindUnlink is harmless here since
	// the local path is fresh.
	cmd := exec.CommandContext(ctx, "ssh", "-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-L", local+":"+remoteSock,
		host)
	// ssh's channel chatter is noise here; the caller reports what matters.
	// MARSHAL_DEBUG=1 shows it.
	if os.Getenv("MARSHAL_DEBUG") != "" {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("ssh to %s: %w", host, err)
	}
	t := &Tunnel{Host: host, Local: local, cmd: cmd}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if c, err := net.DialTimeout("unix", local, 500*time.Millisecond); err == nil {
			c.Close()
			break
		}
		if cmd.ProcessState != nil || time.Now().After(deadline) {
			t.Close()
			return nil, fmt.Errorf("tunnel to %s:%s did not come up; is the daemon running there and is ssh %s working?", host, remoteSock, host)
		}
		time.Sleep(150 * time.Millisecond)
	}
	current = t
	return t, nil
}

// Close ends the tunnel and removes the local socket.
func (t *Tunnel) Close() {
	if t == nil {
		return
	}
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
	}
	_ = os.RemoveAll(filepath.Dir(t.Local))
	mu.Lock()
	if current == t {
		current = nil
	}
	mu.Unlock()
}

// CloseAll tears down the process's tunnel, for exit paths.
func CloseAll() {
	mu.Lock()
	t := current
	mu.Unlock()
	t.Close()
}
