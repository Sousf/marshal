// Package ui is the terminal interface: what the fleet is doing, what each
// run did, where trust stands, and the few controls an operator needs.
package ui

import (
	"context"
	"errors"
	"time"

	"github.com/Sousf/marshal/internal/api"
	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/daemon"
	"github.com/Sousf/marshal/internal/store"
)

// Source is where the interface gets its data. Against a running daemon it
// is the socket; without one it reads the database directly and controls
// that need the daemon report so.
type Source interface {
	Status(ctx context.Context) (api.Status, error)
	Runs(ctx context.Context, repo string, limit int) ([]store.Run, error)
	Run(ctx context.Context, id string) (store.Run, error)
	EventsAfter(ctx context.Context, id string, after int64) ([]store.Event, error)
	Trust(ctx context.Context) ([]store.Trust, error)
	Verdict(ctx context.Context, runID string) (store.Verdict, bool, error)
	Trigger(ctx context.Context, job, repo string) (string, error)
	Fix(ctx context.Context, runID string) (string, error)
	SetAutonomy(ctx context.Context, on bool) error
}

// Daemon talks to a running daemon.
type Daemon struct {
	Client api.Client
}

func (d Daemon) do(ctx context.Context, req api.Request) (api.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return d.Client.Do(ctx, req)
}

func (d Daemon) Status(ctx context.Context) (api.Status, error) {
	r, err := d.do(ctx, api.Request{Op: "status"})
	if err != nil {
		return api.Status{}, err
	}
	return *r.Status, nil
}

func (d Daemon) Runs(ctx context.Context, repo string, limit int) ([]store.Run, error) {
	r, err := d.do(ctx, api.Request{Op: "runs", Repo: repo, Limit: limit})
	return r.Runs, err
}

func (d Daemon) Run(ctx context.Context, id string) (store.Run, error) {
	r, err := d.do(ctx, api.Request{Op: "run", ID: id})
	if err != nil {
		return store.Run{}, err
	}
	return *r.Run, nil
}

func (d Daemon) EventsAfter(ctx context.Context, id string, after int64) ([]store.Event, error) {
	r, err := d.do(ctx, api.Request{Op: "events", ID: id, After: after})
	return r.Events, err
}

func (d Daemon) Trust(ctx context.Context) ([]store.Trust, error) {
	r, err := d.do(ctx, api.Request{Op: "trust"})
	return r.Trust, err
}

func (d Daemon) Verdict(ctx context.Context, runID string) (store.Verdict, bool, error) {
	r, err := d.do(ctx, api.Request{Op: "verdict", ID: runID})
	if err != nil || r.Verdict == nil {
		return store.Verdict{}, false, err
	}
	return *r.Verdict, true, nil
}

func (d Daemon) Trigger(ctx context.Context, job, repo string) (string, error) {
	r, err := d.do(ctx, api.Request{Op: "trigger", Job: job, Repo: repo})
	return r.ID, err
}

func (d Daemon) Fix(ctx context.Context, runID string) (string, error) {
	r, err := d.do(ctx, api.Request{Op: "fix", ID: runID})
	return r.ID, err
}

func (d Daemon) SetAutonomy(ctx context.Context, on bool) error {
	v := "off"
	if on {
		v = "on"
	}
	_, err := d.do(ctx, api.Request{Op: "autonomy", Value: v})
	return err
}

// Local reads the database with no daemon. It is what you get on a laptop
// looking at a copied database, or on the host when the daemon is down.
type Local struct {
	Store *store.Store
	Fleet *config.Fleet
}

// ErrNoDaemon is returned by controls that need the daemon.
var ErrNoDaemon = errors.New("needs the daemon: start it with `marshal serve`")

func (l Local) Status(ctx context.Context) (api.Status, error) {
	spend, _ := l.Store.SpendSince(ctx, time.Now().Add(-24*time.Hour))
	v, _ := l.Store.Setting(ctx, "autonomy", "on")
	return api.Status{Version: "(no daemon)", Autonomy: v == "on", Repos: len(l.Fleet.Repos), Jobs: len(l.Fleet.Jobs), Spend24h: spend}, nil
}

func (l Local) Runs(ctx context.Context, repo string, limit int) ([]store.Run, error) {
	return l.Store.ListRuns(ctx, repo, limit)
}

func (l Local) Run(ctx context.Context, id string) (store.Run, error) {
	return l.Store.GetRun(ctx, id)
}

func (l Local) EventsAfter(ctx context.Context, id string, after int64) ([]store.Event, error) {
	return l.Store.EventsAfter(ctx, id, after)
}

func (l Local) Trust(ctx context.Context) ([]store.Trust, error) {
	rec, err := l.Store.ListTrust(ctx)
	if err != nil {
		return nil, err
	}
	return daemon.Ladder(l.Fleet, rec), nil
}

func (l Local) Verdict(ctx context.Context, runID string) (store.Verdict, bool, error) {
	return l.Store.LatestVerdict(ctx, runID)
}

func (l Local) Trigger(context.Context, string, string) (string, error) {
	return "", ErrNoDaemon
}

func (l Local) Fix(context.Context, string) (string, error) {
	return "", ErrNoDaemon
}

func (l Local) SetAutonomy(ctx context.Context, on bool) error {
	// The setting is read by the daemon on every tick, so writing it here
	// works even though the daemon is what acts on it.
	v := "off"
	if on {
		v = "on"
	}
	return l.Store.SetSetting(ctx, "autonomy", v)
}
