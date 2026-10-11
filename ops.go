package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Sousf/marshal/internal/api"
	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/runner"
	"github.com/Sousf/marshal/internal/store"
)

// agent is the adapter for runs started from this command line: the
// fleet's tool floor applied, and the same quota guard the daemon uses, so
// a foreground run that hits the limit parks the daemon too and a held
// fleet does not start one here either.
func agent(f *config.Fleet, st *store.Store) runner.Adapter {
	return runner.Guard{
		Adapter:   runner.Claude{Never: f.Never.Tools},
		HeldUntil: func(ctx context.Context) time.Time { u, _ := st.QuotaHold(ctx); return u },
		Hold:      func(ctx context.Context, u time.Time, why string) { _ = st.SetQuotaHold(ctx, u, why) },
	}
}

// viaDaemon sends req, and when the daemon is down hands the store to
// local instead: every op here is a database read or write the daemon
// would pick up on its next tick anyway.
func viaDaemon(ctx context.Context, req api.Request, local func(*store.Store) (api.Response, error)) (api.Response, error) {
	resp, err := client().Do(ctx, req)
	if !errors.Is(err, api.ErrDown) {
		return resp, err
	}
	st, err := openStore()
	if err != nil {
		return api.Response{}, err
	}
	defer st.Close()
	return local(st)
}

func cmdIncidents(ctx context.Context, args []string) error {
	all := len(args) == 1 && args[0] == "--all"
	if len(args) > 1 || (len(args) == 1 && !all) {
		return errors.New("usage: marshal incidents [--all]")
	}
	val := ""
	if all {
		val = "all"
	}
	resp, err := viaDaemon(ctx, api.Request{Op: "incidents", Value: val}, func(st *store.Store) (api.Response, error) {
		inc, err := st.ListIncidents(ctx, all)
		return api.Response{Incidents: inc}, err
	})
	if err != nil {
		return err
	}
	if len(resp.Incidents) == 0 {
		fmt.Println("no open incidents")
		return nil
	}
	for _, i := range resp.Incidents {
		state := "open"
		switch {
		case !i.ResolvedAt.IsZero():
			state = "resolved"
		case !i.AckedAt.IsZero():
			state = "acked"
		}
		fmt.Printf("%s  %-8s %s %s  ×%d  last %s ago (run %s)\n    %s\n",
			i.Signature, state, i.Repo, i.Job, i.Count, ago(i.LastAt), i.LastRun, i.Sample)
	}
	return nil
}

func cmdAck(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: marshal ack <signature>")
	}
	resp, err := viaDaemon(ctx, api.Request{Op: "ack", ID: args[0]}, func(st *store.Store) (api.Response, error) {
		inc, err := st.AckIncident(ctx, args[0], time.Now())
		return api.Response{Text: "acked " + inc.Signature + " (" + inc.Repo + " " + inc.Job + ")"}, err
	})
	if err != nil {
		return err
	}
	fmt.Println(resp.Text)
	return nil
}

func cmdQuota(ctx context.Context, args []string) error {
	val := ""
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "clear":
		val = "clear"
	default:
		return errors.New("usage: marshal quota [clear]")
	}
	resp, err := viaDaemon(ctx, api.Request{Op: "quota", Value: val}, func(st *store.Store) (api.Response, error) {
		if val == "clear" {
			return api.Response{Text: "quota hold cleared"}, st.SetQuotaHold(ctx, time.Time{}, "")
		}
		if u, why := st.QuotaHold(ctx); u.After(time.Now()) {
			return api.Response{Text: "held until " + u.Local().Format("Mon Jan 2 15:04") + ": " + why}, nil
		}
		return api.Response{Text: "no quota hold"}, nil
	})
	if err != nil {
		return err
	}
	fmt.Println(resp.Text)
	return nil
}
