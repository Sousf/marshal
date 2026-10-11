// Package notify tells the operator what happened, through whichever
// channel the fleet file names: ntfy, Telegram, or a plain webhook. It is
// deliberately one-way and fire-and-forget: a notification that fails must
// never stall a run, so errors are logged and dropped.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Sousf/marshal/internal/config"
)

// Kind classifies an event so the operator can choose which to receive.
type Kind string

const (
	Proposed   Kind = "proposed"    // a PR was opened
	Merged     Kind = "merged"      // a PR merged, by anyone
	NeedsHuman Kind = "needs-human" // MARSHAL stopped and wants a person
	Failed     Kind = "failed"      // a run failed or was blocked
	Reverted   Kind = "reverted"    // an autonomous merge was reverted
	Digest     Kind = "digest"      // the daily summary
	Test       Kind = "test"        // marshal notify test
)

// Event is one notification.
type Event struct {
	Kind  Kind
	Title string
	Body  string
	URL   string
	// Urgent raises the channel's priority where it has one.
	Urgent bool
}

// Notifier delivers events.
type Notifier interface {
	Send(ctx context.Context, e Event) error
}

// Multi fans out to several channels and filters by kind.
type Multi struct {
	targets []Notifier
	kinds   map[Kind]bool
	// Log receives delivery failures. May be nil.
	Log func(format string, args ...any)
}

// New builds the notifier the fleet configures. nil, nil when nothing is
// configured, so callers can pass it through and check for nil.
func New(cfg config.Notify) (*Multi, error) {
	m := &Multi{kinds: map[Kind]bool{}}
	if cfg.Ntfy.URL != "" {
		m.targets = append(m.targets, Ntfy{URL: cfg.Ntfy.URL, Token: cfg.Ntfy.Token})
	}
	if cfg.Telegram.BotToken != "" || cfg.Telegram.ChatID != "" {
		if cfg.Telegram.BotToken == "" || cfg.Telegram.ChatID == "" {
			return nil, fmt.Errorf("notify.telegram: bot_token and chat_id are both required")
		}
		m.targets = append(m.targets, Telegram{BotToken: cfg.Telegram.BotToken, ChatID: cfg.Telegram.ChatID})
	}
	if cfg.Webhook != "" {
		m.targets = append(m.targets, Webhook{URL: cfg.Webhook})
	}
	if len(m.targets) == 0 {
		return nil, nil
	}
	events := cfg.Events
	if len(events) == 0 {
		events = []string{"proposed", "needs-human", "failed", "reverted", "digest"}
	}
	for _, e := range events {
		m.kinds[Kind(strings.ToLower(strings.TrimSpace(e)))] = true
	}
	m.kinds[Test] = true
	return m, nil
}

// NewForTest builds a Multi over a single target wanting the given kinds.
func NewForTest(t Notifier, kinds ...Kind) *Multi {
	m := &Multi{targets: []Notifier{t}, kinds: map[Kind]bool{}}
	for _, k := range kinds {
		m.kinds[k] = true
	}
	return m
}

// Wants reports whether the operator asked for this kind.
func (m *Multi) Wants(k Kind) bool { return m != nil && m.kinds[k] }

// Send delivers to every target that wants the event. It never returns an
// error for a delivery failure, only for a nil receiver misuse.
func (m *Multi) Send(ctx context.Context, e Event) error {
	if m == nil || !m.Wants(e.Kind) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for _, t := range m.targets {
		if err := t.Send(ctx, e); err != nil && m.Log != nil {
			m.Log("notify: %T: %v", t, err)
		}
	}
	return nil
}

// Ntfy posts to an ntfy topic URL.
type Ntfy struct {
	URL   string
	Token string
	HTTP  *http.Client
}

func (n Ntfy) Send(ctx context.Context, e Event) error {
	body := e.Body
	if e.URL != "" {
		body += "\n" + e.URL
	}
	req, err := http.NewRequestWithContext(ctx, "POST", n.URL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", "MARSHAL: "+e.Title)
	req.Header.Set("Tags", tag(e.Kind))
	if e.Urgent {
		req.Header.Set("Priority", "high")
	}
	if e.URL != "" {
		req.Header.Set("Click", e.URL)
	}
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	return do(client(n.HTTP), req)
}

func tag(k Kind) string {
	switch k {
	case Proposed:
		return "inbox_tray"
	case Merged:
		return "white_check_mark"
	case NeedsHuman:
		return "raising_hand"
	case Failed:
		return "x"
	case Reverted:
		return "rotating_light"
	case Digest:
		return "newspaper"
	}
	return "bell"
}

// Telegram sends through a bot.
type Telegram struct {
	BotToken string
	ChatID   string
	HTTP     *http.Client
	// Base overrides the API host, for tests.
	Base string
}

func (t Telegram) Send(ctx context.Context, e Event) error {
	base := t.Base
	if base == "" {
		base = "https://api.telegram.org"
	}
	text := "*MARSHAL: " + escapeMD(e.Title) + "*\n" + escapeMD(e.Body)
	if e.URL != "" {
		text += "\n" + escapeMD(e.URL)
	}
	form := url.Values{"chat_id": {t.ChatID}, "text": {text}, "parse_mode": {"MarkdownV2"}, "disable_web_page_preview": {"true"}}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/bot"+t.BotToken+"/sendMessage", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return do(client(t.HTTP), req)
}

// escapeMD escapes Telegram MarkdownV2 metacharacters.
func escapeMD(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`_*[]()~`+"`"+`>#+-=|{}.!\`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Webhook posts the event as JSON.
type Webhook struct {
	URL  string
	HTTP *http.Client
}

func (w Webhook) Send(ctx context.Context, e Event) error {
	b, _ := json.Marshal(map[string]any{
		"kind": e.Kind, "title": e.Title, "body": e.Body, "url": e.URL, "urgent": e.Urgent,
		"at": time.Now().UTC().Format(time.RFC3339),
	})
	req, err := http.NewRequestWithContext(ctx, "POST", w.URL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(client(w.HTTP), req)
}

// defaultHTTP is shared so connections are reused rather than a new
// transport being built per notification.
var defaultHTTP = &http.Client{Timeout: 15 * time.Second}

func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return defaultHTTP
}

func do(c *http.Client, req *http.Request) error {
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return nil
}
