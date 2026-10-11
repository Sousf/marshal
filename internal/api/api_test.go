package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sousf/marshal/internal/store"
)

type echo struct{}

func (echo) Handle(_ context.Context, r Request) Response {
	switch r.Op {
	case "status":
		return Response{Status: &Status{Version: "t", Autonomy: true}}
	case "run":
		return Response{Run: &store.Run{ID: r.ID, Status: store.Proposed}}
	case "boom":
		return Fail("no %s", "way")
	}
	return Fail("unknown op %q", r.Op)
}

func TestRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "m.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := Client{Path: sock}
	if c.Up() {
		t.Fatal("nothing should be listening yet")
	}
	go Serve(ctx, sock, echo{})
	deadline := time.Now().Add(2 * time.Second)
	for !c.Up() {
		if time.Now().After(deadline) {
			t.Fatal("daemon never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := c.Do(ctx, Request{Op: "run", ID: "abc"})
	if err != nil || !resp.OK || resp.Run == nil || resp.Run.ID != "abc" {
		t.Errorf("%+v %v", resp, err)
	}
	if _, err := c.Do(ctx, Request{Op: "boom"}); err == nil || err.Error() != "no way" {
		t.Errorf("error not relayed: %v", err)
	}
	if err := Serve(ctx, sock, echo{}); err == nil {
		t.Error("second daemon on the same socket must refuse")
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	if c.Up() {
		t.Error("should be down after cancel")
	}
}
