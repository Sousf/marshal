package ghapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sousf/marshal/internal/config"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestParseKeyBothForms(t *testing.T) {
	k := testKey(t)
	p1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	p8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for _, p := range [][]byte{p1, p8} {
		got, err := ParseKey(p)
		if err != nil || got.N.Cmp(k.N) != 0 {
			t.Errorf("parse: %v", err)
		}
	}
	if _, err := ParseKey([]byte("nope")); err == nil {
		t.Error("garbage should fail")
	}
}

func TestLoad(t *testing.T) {
	if a, err := Load(config.GitHubApp{}); a != nil || err != nil {
		t.Error("empty block means no app")
	}
	if _, err := Load(config.GitHubApp{AppID: 1}); err == nil {
		t.Error("partial block must fail")
	}
	k := testKey(t)
	path := filepath.Join(t.TempDir(), "k.pem")
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600)
	a, err := Load(config.GitHubApp{AppID: 7, InstallationID: 9, PrivateKeyFile: path, Slug: "marshal-x"})
	if err != nil || a.AppID != 7 || a.InstallationID != 9 {
		t.Fatal(err)
	}
	id := a.Identity()
	if id.Name != "marshal-x[bot]" || id.Email != "7+marshal-x[bot]@users.noreply.github.com" {
		t.Errorf("%+v", id)
	}
}

func TestJWT(t *testing.T) {
	a := &App{AppID: 42, Key: testKey(t), Now: func() time.Time { return time.Unix(1_000_000, 0) }}
	jwt, err := a.JWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("parts = %d", len(parts))
	}
	hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if string(hdr) != `{"alg":"RS256","typ":"JWT"}` {
		t.Errorf("header %s", hdr)
	}
	var claims map[string]any
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(raw, &claims)
	if claims["iss"] != "42" || claims["iat"].(float64) != 1_000_000-60 || claims["exp"].(float64) != 1_000_000+540 {
		t.Errorf("claims %v", claims)
	}
}

func TestTokenEnvAndCache(t *testing.T) {
	calls := 0
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/app/installations/9/access_tokens" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			t.Errorf("bad request %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		lastBody = string(b)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_test" + string(rune('0'+calls)), "expires_at": time.Now().Add(time.Hour)})
	}))
	defer srv.Close()
	a := &App{AppID: 1, InstallationID: 9, Key: testKey(t), BaseURL: srv.URL}
	ctx := context.Background()
	full, err := a.Token(ctx, Full)
	if err != nil || full != "ghs_test1" || lastBody != "" {
		t.Fatalf("full: %q %v body=%q", full, err, lastBody)
	}
	ro, err := a.Token(ctx, ReadOnly)
	if err != nil || ro != "ghs_test2" || !strings.Contains(lastBody, `"contents":"read"`) || strings.Contains(lastBody, "write") {
		t.Fatalf("ro: %q %v body=%q", ro, err, lastBody)
	}
	if again, _ := a.Token(ctx, Full); again != full || calls != 2 {
		t.Error("second call should hit the cache")
	}
	env, err := a.Env(ctx, Full)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GH_TOKEN=ghs_test1") || !strings.Contains(joined, "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader") {
		t.Errorf("env:\n%s", joined)
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_test1"))
	if !strings.Contains(joined, "AUTHORIZATION: basic "+basic) {
		t.Error("git header wrong")
	}
}

func TestTokenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	a := &App{AppID: 1, InstallationID: 9, Key: testKey(t), BaseURL: srv.URL}
	if _, err := a.Token(context.Background(), Full); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v", err)
	}
}

func TestCloneURL(t *testing.T) {
	if got := CloneURL(config.Repo{URL: "git@github.com:Sousf/ears.git"}); got != "https://github.com/Sousf/ears.git" {
		t.Error(got)
	}
	if got := CloneURL(config.Repo{URL: "/local/mirror", GitHub: "Sousf/x"}); got != "https://github.com/Sousf/x.git" {
		t.Error(got)
	}
	if got := CloneURL(config.Repo{URL: "/local/only"}); got != "/local/only" {
		t.Error(got)
	}
}
