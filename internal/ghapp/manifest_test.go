package ghapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCreateFlow drives the manifest flow with a fake GitHub: the browser
// steps are simulated by hitting the local callback and setup URLs.
func TestCreateFlow(t *testing.T) {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/app-manifests/thecode/conversions" {
			http.Error(w, "wrong call "+r.URL.Path, 404)
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"id": 4242, "slug": "marshal-test", "pem": pemText, "html_url": "https://github.com/apps/marshal-test"})
	}))
	defer api.Close()

	keyPath := filepath.Join(t.TempDir(), "sub", "app.pem")
	var opened string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		c   Created
		err error
	}
	res := make(chan result, 1)
	ready := make(chan struct{})
	go func() {
		c, err := Create(ctx, CreateOptions{
			Name: "marshal-test", Homepage: "https://github.com/Sousf/marshal", KeyPath: keyPath,
			GitHubURL: "https://github.example", APIURL: api.URL,
			Open: func(u string) { opened = u; close(ready) },
		})
		res <- result{c, err}
	}()
	<-ready

	// The landing page carries the manifest and the state.
	resp, err := http.Get(opened)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	if !strings.Contains(page, "https://github.example/settings/apps/new?state=") || !strings.Contains(page, `name="manifest"`) {
		t.Fatalf("landing page:\n%s", page)
	}
	state := strings.SplitN(strings.SplitN(page, "state=", 2)[1], `"`, 2)[0]
	if !strings.Contains(page, `&#34;workflows&#34;:&#34;write&#34;`) || !strings.Contains(page, `&#34;active&#34;:false`) {
		t.Error("manifest should ask for workflows:write and no webhook")
	}
	if !strings.Contains(page, `&#34;hook_attributes&#34;:{&#34;active&#34;:false,&#34;url&#34;:`) {
		t.Error("GitHub requires hook_attributes.url even when the hook is inactive")
	}
	base := strings.TrimSuffix(opened, "/")

	// A callback with the wrong state is refused and does not end the flow.
	if r, _ := http.Get(base + "/callback?code=thecode&state=nope"); r.StatusCode != 400 {
		t.Errorf("bad state status %d", r.StatusCode)
	}
	select {
	case r := <-res:
		t.Fatalf("flow ended early: %+v %v", r.c, r.err)
	case <-time.After(100 * time.Millisecond):
	}

	// The real callback: key written, redirect to install.
	r, err := http.Get(base + "/callback?code=thecode&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(b), "apps/marshal-test/installations/new") {
		t.Fatalf("callback %d:\n%s", r.StatusCode, b)
	}
	if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", info, err)
	}
	if got, _ := os.ReadFile(keyPath); string(got) != pemText {
		t.Error("key contents differ")
	}

	// The install redirect finishes it.
	if r, _ := http.Get(base + "/installed?installation_id=777&setup_action=install"); r.StatusCode != 200 {
		t.Errorf("installed status %d", r.StatusCode)
	}
	got := <-res
	if got.err != nil || got.c.AppID != 4242 || got.c.Slug != "marshal-test" || got.c.InstallationID != 777 || got.c.KeyPath != keyPath {
		t.Errorf("%+v %v", got.c, got.err)
	}
}

func TestWriteKeyRefusesOverwrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.pem")
	if err := writeKey(p, "a"); err != nil {
		t.Fatal(err)
	}
	if err := writeKey(p, "b"); err == nil {
		t.Error("must not overwrite an existing key")
	}
}
