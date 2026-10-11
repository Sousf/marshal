package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Sousf/marshal/internal/notify"
	"github.com/Sousf/marshal/internal/run"
	"github.com/Sousf/marshal/internal/store"
)

// A daemon on the operator's own machine upgrades the tools installed there
// from the tap the moment a release lands: it saw the release PR merge, so
// it knows a version is coming, and it watches the tap for it instead of
// anything polling on a timer. When the tool is MARSHAL, the daemon then
// restarts into the new binary once nothing is running.

// awaitPrefix keys the settings rows of releases merged and not yet seen
// in the tap: one per repo at most, so the table stays bounded.
const awaitPrefix = "release-await:"

// awaitFor is how long a merged release may take to reach the tap before
// the operator is told the release workflow probably failed.
const awaitFor = 2 * time.Hour

var releaseTitle = regexp.MustCompile(`^Release v(\d+\.\d+\.\d+)\b`)

// expectRelease records that a release PR merged, so its version is
// watched for in the tap.
func (d *Daemon) expectRelease(ctx context.Context, r store.Run) {
	if !d.Fleet.Homebrew.UpgradeLocal || r.Job != run.ReleaseJob {
		return
	}
	m := releaseTitle.FindStringSubmatch(firstLine(r.Summary))
	if m == nil {
		return
	}
	_ = d.Store.SetSetting(ctx, awaitPrefix+r.Repo, m[1]+"@"+strconv.FormatInt(d.now().Unix(), 10))
	d.Log.Printf("%s: release v%s merged; upgrading here once it reaches the tap", r.Repo, m[1])
}

// upgradeReleased checks each awaited release against the tap and upgrades
// the ones that have landed. The brew work happens in the background; the
// tick only looks.
func (d *Daemon) upgradeReleased(ctx context.Context) {
	h := d.Fleet.Homebrew
	owner, tap, _, ok := h.TapParts()
	if !h.UpgradeLocal || !ok {
		return
	}
	d.mu.Lock()
	busy := d.upgrading
	d.mu.Unlock()
	if busy {
		return
	}
	for _, repo := range d.Fleet.Repos {
		key := awaitPrefix + repo.Name
		v, _ := d.Store.Setting(ctx, key, "")
		ver, since, ok := strings.Cut(v, "@")
		if !ok {
			continue
		}
		sec, _ := strconv.ParseInt(since, 10, 64)
		if d.now().Sub(time.Unix(sec, 0)) > awaitFor {
			_ = d.Store.SetSetting(ctx, key, "")
			d.send(ctx, notify.Event{Kind: notify.Failed, Title: repo.Name + ": v" + ver + " never reached the tap",
				Body: "The release PR merged " + awaitFor.String() + " ago; check the release workflow's run."})
			continue
		}
		formula, err := d.GH.FileText(ctx, owner+"/"+tap, "Formula/"+repo.Name+".rb")
		if err != nil || !formulaHas(formula, ver) {
			continue // not there yet; look again next tick
		}
		_ = d.Store.SetSetting(ctx, key, "")
		d.mu.Lock()
		d.upgrading = true
		d.mu.Unlock()
		go func(name, ver string) {
			defer func() {
				d.mu.Lock()
				d.upgrading = false
				d.mu.Unlock()
			}()
			d.brewUpgrade(d.base, name, ver)
		}(repo.Name, ver)
		return // one at a time; the rest next tick
	}
}

// formulaHas reports whether a formula is at version: GoReleaser writes
// version "X.Y.Z"; a source formula names the tag's tarball.
func formulaHas(formula, ver string) bool {
	return strings.Contains(formula, `version "`+ver+`"`) || strings.Contains(formula, "/v"+ver+".tar.gz")
}

// brewUpgrade upgrades one Sousf/tap formula if it is installed here.
func (d *Daemon) brewUpgrade(ctx context.Context, name, ver string) {
	owner, tap, _, _ := d.Fleet.Homebrew.TapParts()
	full := strings.ToLower(owner + "/" + strings.TrimPrefix(tap, "homebrew-") + "/" + name)
	brew := d.Fleet.Homebrew.BrewPath()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	out, err := d.brew(ctx, brew, "list", "--formula", "--full-name")
	if err != nil {
		d.fail("brew list: %v", err)
		return
	}
	if !strings.Contains("\n"+out+"\n", "\n"+full+"\n") {
		d.Log.Printf("%s v%s is in the tap; not installed here with brew, nothing to upgrade", name, ver)
		return
	}
	if _, err := d.brew(ctx, brew, "update", "--quiet"); err != nil {
		d.fail("brew update: %v", err)
		return
	}
	if out, err := d.brew(ctx, brew, "upgrade", "--formula", full); err != nil {
		d.fail("brew upgrade %s: %v: %s", full, err, lastLines(out, 5))
		return
	}
	d.Log.Printf("upgraded %s to v%s here", name, ver)
	if name == "marshal" {
		d.mu.Lock()
		d.restartTo = filepath.Join(filepath.Dir(brew), "marshal")
		d.mu.Unlock()
		d.Log.Printf("restarting into marshal v%s once nothing is running", ver)
	}
}

func (d *Daemon) brew(ctx context.Context, brew string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, brew, args...)
	cmd.Env = append(os.Environ(), "HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ENV_HINTS=1", "NONINTERACTIVE=1")
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if len(out) > 64*1024 {
		out = out[len(out)-64*1024:]
	}
	return string(out), err
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// maybeRestart hands over to a newly installed MARSHAL when nothing is in
// flight. Restart is supplied by the command that started the daemon,
// which owns the process; without it the upgrade waits for a manual one.
func (d *Daemon) maybeRestart() {
	d.mu.Lock()
	to := d.restartTo
	idle := len(d.running) == 0 && len(d.verifying) == 0 && !d.upgrading
	d.mu.Unlock()
	if to == "" || !idle || d.Restart == nil {
		return
	}
	d.Log.Printf("restarting into %s", to)
	d.Restart(to)
}
