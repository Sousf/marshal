// Package api is the wire between the daemon and anything that wants to
// talk to it: the CLI, the TUI. One JSON object per line over a Unix socket.
//
// A Unix socket because the daemon has no business listening on a network
// port. Reaching it from another machine is ssh's job, which brings the
// authentication with it. OpenClaw's default of binding to every interface
// is the cautionary tale here.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/Sousf/marshal/internal/store"
)

// Request is one call to the daemon.
type Request struct {
	// Op is one of: status, runs, run, events, trigger, task, fix, autonomy,
	// trust, verdict, log, incidents, ack, quota. For ack, ID is a signature
	// prefix; for quota, Value "clear" lifts the hold; for incidents, Value
	// "all" includes resolved ones.
	// For task, Value is the instruction, Repo optional, Job an optional name.
	Op    string `json:"op"`
	Repo  string `json:"repo,omitempty"`
	Job   string `json:"job,omitempty"`
	ID    string `json:"id,omitempty"`
	After int64  `json:"after,omitempty"`
	Limit int    `json:"limit,omitempty"`
	// Value is the argument for autonomy: "on" or "off".
	Value string `json:"value,omitempty"`
}

// Status is the daemon's self-description.
type Status struct {
	Version   string    `json:"version"`
	Started   time.Time `json:"started"`
	Autonomy  bool      `json:"autonomy"`
	Running   []string  `json:"running"`   // run IDs in flight
	Verifying []string  `json:"verifying"` // run IDs being verified
	Repos     int       `json:"repos"`
	Jobs      int       `json:"jobs"`
	Spend24h  float64   `json:"spend_24h"`
	LastTick  time.Time `json:"last_tick"`
	LastError string    `json:"last_error,omitempty"`
	// QuotaUntil is set while a quota hold parks every agent run.
	QuotaUntil time.Time `json:"quota_until,omitempty"`
	QuotaWhy   string    `json:"quota_why,omitempty"`
	// Incidents counts open incidents nobody has acked.
	Incidents int `json:"incidents,omitempty"`
}

// Response is the daemon's answer.
type Response struct {
	OK     bool          `json:"ok"`
	Error  string        `json:"error,omitempty"`
	Status *Status       `json:"status,omitempty"`
	Runs   []store.Run   `json:"runs,omitempty"`
	Run    *store.Run    `json:"run,omitempty"`
	Events []store.Event `json:"events,omitempty"`
	Trust  []store.Trust `json:"trust,omitempty"`
	// Incidents answers the incidents op.
	Incidents []store.Incident `json:"incidents,omitempty"`
	// Verdict is the latest verifier verdict for a run, when one exists.
	Verdict *store.Verdict `json:"verdict,omitempty"`
	// ID is the run started by trigger.
	ID string `json:"id,omitempty"`
	// Text carries free text, such as the daemon's log tail.
	Text string `json:"text,omitempty"`
}

// Fail builds an error response.
func Fail(format string, args ...any) Response {
	return Response{Error: fmt.Sprintf(format, args...)}
}

// Handler answers requests.
type Handler interface {
	Handle(ctx context.Context, req Request) Response
}

// Serve listens on a Unix socket until ctx is done. A stale socket file from
// a previous daemon is removed first, after checking nothing answers on it.
func Serve(ctx context.Context, path string, h Handler) error {
	if (Client{Path: path}).Up() {
		return fmt.Errorf("another daemon is already listening on %s", path)
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	// Only the owner may talk to the daemon.
	_ = os.Chmod(path, 0o600)
	go func() {
		<-ctx.Done()
		ln.Close()
		os.Remove(path)
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go serveConn(ctx, conn, h)
	}
}

func serveConn(ctx context.Context, conn net.Conn, h Handler) {
	defer conn.Close()
	// A client that connects and says nothing must not pin a goroutine for
	// the daemon's lifetime.
	_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	enc := json.NewEncoder(conn)
	for sc.Scan() {
		var req Request
		var resp Response
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			resp = Fail("bad request: %v", err)
		} else {
			resp = h.Handle(ctx, req)
			if resp.Error == "" {
				resp.OK = true
			}
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
	}
}

// Client talks to a daemon.
type Client struct {
	Path string
}

// ErrDown is returned when no daemon is listening.
var ErrDown = errors.New("daemon is not running")

// Up reports whether a daemon answers on the socket.
func (c Client) Up() bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := c.Do(ctx, Request{Op: "status"})
	return err == nil && resp.OK
}

// Do sends one request and reads one response.
func (c Client) Do(ctx context.Context, req Request) (Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return Response{}, fmt.Errorf("%w (%s)", ErrDown, c.Path)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("daemon: %w", err)
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}
