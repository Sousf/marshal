package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Sousf/marshal/internal/config"
)

func TestNewFilters(t *testing.T) {
	if m, err := New(config.Notify{}); m != nil || err != nil {
		t.Error("nothing configured means no notifier")
	}
	if _, err := New(config.Notify{Telegram: config.Telegram{BotToken: "x"}}); err == nil {
		t.Error("half a telegram config must fail")
	}
	m, err := New(config.Notify{Ntfy: config.Ntfy{URL: "http://x"}, Events: []string{"proposed", "Reverted"}})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Wants(Proposed) || !m.Wants(Reverted) || m.Wants(Merged) || !m.Wants(Test) {
		t.Error("event filter wrong")
	}
	d, _ := New(config.Notify{Webhook: "http://x"})
	if !d.Wants(NeedsHuman) || !d.Wants(Digest) || d.Wants(Merged) {
		t.Error("defaults: everything but merged")
	}
}

func TestChannels(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.URL.Path+"|"+r.Header.Get("Title")+"|"+r.Header.Get("Priority")+"|"+string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	e := Event{Kind: NeedsHuman, Title: "PR needs you", Body: "system-patch #3", URL: "https://github.com/x/y/pull/3", Urgent: true}

	if err := (Ntfy{URL: srv.URL + "/topic"}).Send(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if err := (Telegram{BotToken: "tok", ChatID: "42", Base: srv.URL}).Send(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if err := (Webhook{URL: srv.URL + "/hook"}).Send(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("%d deliveries", len(got))
	}
	if !strings.HasPrefix(got[0], "/topic|MARSHAL: PR needs you|high|system-patch #3\nhttps://") {
		t.Errorf("ntfy: %s", got[0])
	}
	if !strings.HasPrefix(got[1], "/bottok/sendMessage|") || !strings.Contains(got[1], "chat_id=42") || !strings.Contains(got[1], `system%5C-patch`) {
		t.Errorf("telegram: %s", got[1])
	}
	var hook map[string]any
	json.Unmarshal([]byte(strings.SplitN(got[2], "|", 4)[3]), &hook)
	if hook["kind"] != "needs-human" || hook["urgent"] != true {
		t.Errorf("webhook: %v", hook)
	}
}

func TestMultiSwallowsFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 500) }))
	defer srv.Close()
	var logged []string
	m, _ := New(config.Notify{Webhook: srv.URL})
	m.Log = func(f string, a ...any) { logged = append(logged, f) }
	if err := m.Send(context.Background(), Event{Kind: Failed, Title: "t"}); err != nil {
		t.Fatal("delivery failure must not surface as an error")
	}
	if len(logged) != 1 {
		t.Error("failure should be logged")
	}
	if err := m.Send(context.Background(), Event{Kind: Merged}); err != nil || len(logged) != 1 {
		t.Error("unwanted kinds are dropped silently")
	}
	var nilm *Multi
	if nilm.Send(context.Background(), Event{Kind: Failed}) != nil || nilm.Wants(Failed) {
		t.Error("nil notifier is a no-op")
	}
}
