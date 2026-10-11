// Package ghapp authenticates MARSHAL as a GitHub App installation, so its
// commits, pull requests and merges carry the app's identity rather than the
// operator's, and so the token the agent sees is a short-lived read-only one
// while the harness holds the one that can write.
//
// The flow is the standard one: sign a JWT with the app's private key, trade
// it for an installation access token, cache that until shortly before it
// expires. Everything here is standard library.
package ghapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/github"
)

// Scope is how much a token may do.
type Scope int

const (
	// Full is everything the installation was granted.
	Full Scope = iota
	// ReadOnly is what the agent gets: enough to read the repo, CI logs and
	// PRs through gh, nothing that writes.
	ReadOnly
)

var readOnlyPermissions = map[string]string{
	"contents":      "read",
	"metadata":      "read",
	"pull_requests": "read",
	"actions":       "read",
	"checks":        "read",
	"issues":        "read",
}

// App is a loaded installation.
type App struct {
	AppID          int64
	InstallationID int64
	Slug           string
	Key            *rsa.PrivateKey
	// BaseURL defaults to the public API.
	BaseURL string
	HTTP    *http.Client
	Now     func() time.Time

	mu    sync.Mutex
	cache map[Scope]token
}

type token struct {
	value   string
	expires time.Time
}

// Load reads the private key and builds an App. It returns nil, nil when the
// fleet has no github_app block, so callers can pass the result through.
func Load(cfg config.GitHubApp) (*App, error) {
	if cfg.AppID == 0 && cfg.InstallationID == 0 && cfg.PrivateKeyFile == "" {
		return nil, nil
	}
	if cfg.AppID == 0 || cfg.InstallationID == 0 || cfg.PrivateKeyFile == "" {
		return nil, errors.New("github_app: app_id, installation_id and private_key_file are all required")
	}
	path := cfg.PrivateKeyFile
	if strings.HasPrefix(path, "~/") {
		path = filepath.Join(os.Getenv("HOME"), path[2:])
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("github_app: %w", err)
	}
	key, err := ParseKey(b)
	if err != nil {
		return nil, fmt.Errorf("github_app: %s: %w", path, err)
	}
	return &App{AppID: cfg.AppID, InstallationID: cfg.InstallationID, Slug: cfg.Slug, Key: key}, nil
}

// ParseKey accepts the PEM GitHub hands out (PKCS#1) and PKCS#8.
func ParseKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("not a PEM file")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rk, nil
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) base() string {
	if a.BaseURL != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	return "https://api.github.com"
}

func (a *App) http() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// JWT signs the app assertion. Issued a minute in the past to absorb clock
// skew, valid for nine minutes; GitHub rejects anything over ten.
func (a *App) JWT() (string, error) {
	now := a.now()
	header := b64(`{"alg":"RS256","typ":"JWT"}`)
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.AppID, 10),
	})
	signing := header + "." + b64(string(claims))
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// Token returns an installation token for the scope, minting one when the
// cached token has under five minutes left.
func (a *App) Token(ctx context.Context, scope Scope) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cache == nil {
		a.cache = map[Scope]token{}
	}
	if t, ok := a.cache[scope]; ok && a.now().Before(t.expires.Add(-5*time.Minute)) {
		return t.value, nil
	}
	jwt, err := a.JWT()
	if err != nil {
		return "", err
	}
	var body io.Reader
	if scope == ReadOnly {
		b, _ := json.Marshal(map[string]any{"permissions": readOnlyPermissions})
		body = strings.NewReader(string(b))
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.base(), a.InstallationID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http().Do(req)
	if err != nil {
		return "", fmt.Errorf("github app token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github app token: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("github app token: unexpected response")
	}
	a.cache[scope] = token{value: out.Token, expires: out.ExpiresAt}
	return out.Token, nil
}

// Env returns the environment that makes gh and git act as the app: GH_TOKEN
// for gh, and an HTTP auth header for git over https, set through
// GIT_CONFIG_* so no token lands in any file.
func (a *App) Env(ctx context.Context, scope Scope) ([]string, error) {
	tok, err := a.Token(ctx, scope)
	if err != nil {
		return nil, err
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	return []string{
		"GH_TOKEN=" + tok,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + basic,
	}, nil
}

// Identity is how the app's commits are attributed, matching what GitHub
// shows for an app's own commits so they link to the app.
func (a *App) Identity() config.Identity {
	slug := a.Slug
	if slug == "" {
		slug = "marshal"
	}
	return config.Identity{
		Name:  slug + "[bot]",
		Email: fmt.Sprintf("%d+%s[bot]@users.noreply.github.com", a.AppID, slug),
	}
}

// CloneURL rewrites a GitHub repo's URL to https so the token header
// applies. Non-GitHub URLs are returned unchanged.
func CloneURL(repo config.Repo) string {
	slug := repo.GitHub
	if slug == "" {
		s, ok := github.Slug(repo.URL)
		if !ok {
			return repo.URL
		}
		slug = s
	}
	return "https://github.com/" + slug + ".git"
}
