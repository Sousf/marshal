package ghapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GitHub cannot create an app from the command line, but it can create one
// from a manifest submitted by a browser form, and it tells the submitter
// the result through a redirect. Create runs that flow against a server on
// localhost: the operator confirms twice in the browser, once to create and
// once to install, and the app id, private key and installation id arrive
// here without being copied by hand.

// CreateOptions configure the flow.
type CreateOptions struct {
	// Name is the app's display name and, lowercased, its slug. GitHub
	// requires it to be unique across all of GitHub.
	Name string
	// Homepage is required by GitHub; the MARSHAL repo is a fine answer.
	Homepage string
	// KeyPath is where the private key is written, mode 0600.
	KeyPath string
	// Addr is the listen address; empty means 127.0.0.1 on a free port.
	Addr string
	// GitHubURL and APIURL default to github.com; tests point them elsewhere.
	GitHubURL string
	APIURL    string
	// Open is called with the URL the operator must visit. Nil prints it.
	Open func(url string)
	// Out receives progress lines.
	Out io.Writer
}

// Created is what the flow produced.
type Created struct {
	AppID          int64
	InstallationID int64
	Slug           string
	HTMLURL        string
	KeyPath        string
}

// manifest is the body GitHub expects, documented at
// docs.github.com/apps/sharing-github-apps/registering-a-github-app-from-a-manifest.
type manifest struct {
	Name           string            `json:"name"`
	URL            string            `json:"url"`
	RedirectURL    string            `json:"redirect_url"`
	SetupURL       string            `json:"setup_url"`
	SetupOnUpdate  bool              `json:"setup_on_update"`
	Public         bool              `json:"public"`
	HookAttributes map[string]any    `json:"hook_attributes"`
	Permissions    map[string]string `json:"default_permissions"`
	Events         []string          `json:"default_events"`
}

// Permissions are the least MARSHAL needs: read and write code, PRs and
// labels (issues), write workflows so onboarding can add ci.yml, read
// Actions and checks for CI status.
var Permissions = map[string]string{
	"contents":      "write",
	"pull_requests": "write",
	"issues":        "write",
	"workflows":     "write",
	"actions":       "read",
	"checks":        "read",
	"metadata":      "read",
}

// Create runs the manifest flow until the app is created and installed, or
// ctx ends. Partial progress is returned with the error so the caller can
// record an app that was created but not yet installed.
func Create(ctx context.Context, opts CreateOptions) (Created, error) {
	if opts.Name == "" {
		return Created{}, errors.New("app name is required")
	}
	if opts.GitHubURL == "" {
		opts.GitHubURL = "https://github.com"
	}
	if opts.APIURL == "" {
		opts.APIURL = "https://api.github.com"
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	addr := opts.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return Created{}, err
	}
	defer ln.Close()
	base := "http://" + ln.Addr().String()

	stateBytes := make([]byte, 16)
	rand.Read(stateBytes)
	state := hex.EncodeToString(stateBytes)

	m := manifest{
		Name:        opts.Name,
		URL:         opts.Homepage,
		RedirectURL: base + "/callback",
		SetupURL:    base + "/installed",
		Public:      false,
		// GitHub requires a webhook url in the manifest even with the hook
		// inactive, and reports its absence as `"url" wasn't supplied`,
		// which reads as the homepage. Nothing is ever delivered to it.
		HookAttributes: map[string]any{"active": false, "url": opts.Homepage},
		Permissions:    Permissions,
		Events:         []string{},
	}
	mjson, _ := json.Marshal(m)

	var created Created
	created.KeyPath = opts.KeyPath
	done := make(chan error, 1)
	finish := func(err error) {
		select {
		case done <- err:
		default:
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		action := opts.GitHubURL + "/settings/apps/new?state=" + state
		fmt.Fprintf(w, page("Creating the MARSHAL GitHub App",
			`<p>Sending the app definition to GitHub. If nothing happens, press the button.</p>
<form id="f" method="post" action="%s"><input type="text" name="manifest" value="%s" style="display:none">
<button type="submit">Create GitHub App</button></form><script>document.getElementById("f").submit()</script>`),
			html.EscapeString(action), html.EscapeString(string(mjson)))
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		// A wrong state is refused but does not end the flow: anything on
		// the machine can hit a localhost port, and the real redirect may
		// still be coming.
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch; this is not the redirect MARSHAL is waiting for", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		conv, err := convert(r.Context(), opts.APIURL, code)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			finish(err)
			return
		}
		if err := writeKey(opts.KeyPath, conv.PEM); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			finish(err)
			return
		}
		created.AppID = conv.ID
		created.Slug = conv.Slug
		created.HTMLURL = conv.HTMLURL
		fmt.Fprintf(opts.Out, "created app %s (id %d); private key saved to %s\n", conv.Slug, conv.ID, opts.KeyPath)
		install := conv.HTMLURL + "/installations/new"
		fmt.Fprintf(w, page("App created",
			`<p>App <b>%s</b> created and its key saved. Now install it on the repositories MARSHAL looks after.</p>
<p><a href="%s">Install %s</a></p><script>location.href=%q</script>`),
			html.EscapeString(conv.Slug), html.EscapeString(install), html.EscapeString(conv.Slug), install)
	})
	mux.HandleFunc("/installed", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "no installation_id in the redirect", http.StatusBadRequest)
			return
		}
		created.InstallationID = id
		fmt.Fprintf(opts.Out, "installed as installation %d\n", id)
		fmt.Fprint(w, page("Done", `<p>MARSHAL is installed. You can close this tab; the terminal has the rest.</p>`))
		finish(nil)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	if opts.Open != nil {
		opts.Open(base + "/")
	} else {
		fmt.Fprintf(opts.Out, "open %s/ in your browser\n", base)
	}
	select {
	case err := <-done:
		return created, err
	case <-ctx.Done():
		return created, ctx.Err()
	}
}

type conversion struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	PEM     string `json:"pem"`
	HTMLURL string `json:"html_url"`
}

// convert trades the one-time code for the app's credentials. No auth: the
// code is the credential, and it expires in an hour.
func convert(ctx context.Context, api, code string) (conversion, error) {
	if code == "" {
		return conversion{}, errors.New("callback carried no code")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(api, "/")+"/app-manifests/"+code+"/conversions", nil)
	if err != nil {
		return conversion{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return conversion{}, fmt.Errorf("manifest conversion: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return conversion{}, fmt.Errorf("manifest conversion: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var c conversion
	if err := json.Unmarshal(raw, &c); err != nil || c.ID == 0 || c.PEM == "" {
		return conversion{}, errors.New("manifest conversion: unexpected response")
	}
	return c, nil
}

func writeKey(path, pem string) error {
	if path == "" {
		return errors.New("no key path")
	}
	if strings.HasPrefix(path, "~/") {
		path = filepath.Join(os.Getenv("HOME"), path[2:])
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; move it aside first", path)
	}
	return os.WriteFile(path, []byte(pem), 0o600)
}

func page(title, body string) string {
	return `<!doctype html><meta charset="utf-8"><title>` + html.EscapeString(title) + `</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#222}
button,a{font:inherit;padding:.5rem 1rem;background:#5a32d6;color:#fff;border:0;border-radius:6px;text-decoration:none}</style>
<h1>` + html.EscapeString(title) + `</h1>` + body
}
