package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// HostOptions fills in what a run needs from this host once hide has
// emptied the home directory: the PATH directories and Go toolchain that
// live under a hidden directory, read-only, and the Go caches, read-write.
// Without the caches every verb would download and compile from scratch.
func HostOptions(hide, mask, readOnly, writable []string) Options {
	opt := Options{Hide: hide, Mask: mask}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		// A PATH entry that is a hidden root, or holds one, would
		// undo the hiding.
		if filepath.IsAbs(d) && UnderAny(d, hide) && !covers(d, hide) && !covers(d, mask) {
			opt.ReadOnly = append(opt.ReadOnly, filepath.Clean(d))
		}
	}
	if _, err := exec.LookPath("go"); err == nil {
		if out, err := exec.Command("go", "env", "GOROOT", "GOENV", "GOCACHE", "GOMODCACHE").Output(); err == nil {
			v := strings.Split(strings.TrimSpace(string(out)), "\n")
			if len(v) == 4 {
				opt.ReadOnly = appendAbs(opt.ReadOnly, v[0], v[1])
				opt.Writable = appendAbs(opt.Writable, v[2], v[3])
			}
		}
	}
	opt.ReadOnly = append(opt.ReadOnly, readOnly...)
	opt.Writable = append(opt.Writable, writable...)
	return opt
}

// covers reports whether d is any of paths or one of their ancestors.
func covers(d string, paths []string) bool {
	for _, p := range paths {
		if UnderAny(p, []string{d}) {
			return true
		}
	}
	return false
}

func appendAbs(list []string, paths ...string) []string {
	for _, p := range paths {
		if filepath.IsAbs(p) {
			list = append(list, filepath.Clean(p))
		}
	}
	return list
}
