package verifier

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Sousf/marshal/internal/contract"
)

// Bounds on the module comparison. A dependency bump whose upstream source
// change does not fit is not judged: the verdict is UNSURE and a human
// reads it.
const (
	// MaxModuleDiff is the most upstream diff, across every module a change
	// bumps, that goes in front of the verifier.
	MaxModuleDiff = 160 << 10
	// MaxModules is the most modules one change may bump and still be
	// compared.
	MaxModules = 12
	// maxSourceFile skips a single generated or vendored file so one table
	// cannot exhaust memory during the scan.
	maxSourceFile = 2 << 20
)

// Bump is one module whose required version changed.
type Bump struct {
	Path, From, To string
}

// ModuleDiff is what the harness established about the upstream source of
// the modules a change bumps. Diff is untrusted text written by whoever
// controls those modules.
type ModuleDiff struct {
	Bumps []Bump
	// Flags are capabilities a new version gains over the old one.
	Flags []string
	// Unchecked says why some or all of the source could not be compared.
	// Any entry makes a PASS an UNSURE.
	Unchecked []string
	Diff      string
}

var versionRE = regexp.MustCompile(`^\s*(?:require\s+)?([A-Za-z0-9._~/-]+\.[A-Za-z0-9._~/-]+)\s+(v[0-9][^\s/]*)`)

// Bumps lists the modules required at both base and head whose version
// changed, sorted by path. New modules are not here: Run reports them, and
// they fail the verdict on their own.
func Bumps(baseGoMod, headGoMod string) []Bump {
	before, after := versions(baseGoMod), versions(headGoMod)
	var out []Bump
	for mod, to := range after {
		if from, ok := before[mod]; ok && from != to {
			out = append(out, Bump{Path: mod, From: from, To: to})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func versions(gomod string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(gomod))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "replace ") || strings.HasPrefix(line, "exclude ") || strings.HasPrefix(line, "retract ") {
			continue
		}
		if m := versionRE.FindStringSubmatch(line); m != nil {
			out[m[1]] = m[2]
		}
	}
	return out
}

// CompareModules downloads both versions of every bumped module through the
// module proxy, checked against go.sum's checksum database as the go command
// always does, and diffs them. It runs in the harness, never in the agent,
// and executes nothing from the modules.
func CompareModules(ctx context.Context, bumps []Bump) ModuleDiff {
	md := ModuleDiff{Bumps: bumps}
	if len(bumps) == 0 {
		return md
	}
	if len(bumps) > MaxModules {
		md.Unchecked = append(md.Unchecked, fmt.Sprintf("%d modules bumped; the source comparison stops at %d", len(bumps), MaxModules))
		return md
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	dirs, err := download(ctx, bumps)
	if err != nil {
		md.Unchecked = append(md.Unchecked, "module download: "+err.Error())
		return md
	}
	var diff strings.Builder
	for _, b := range bumps {
		old, cur := dirs[b.Path+"@"+b.From], dirs[b.Path+"@"+b.To]
		if old == "" || cur == "" {
			md.Unchecked = append(md.Unchecked, fmt.Sprintf("%s: source not downloaded", b.Path))
			continue
		}
		md.Flags = append(md.Flags, gained(b, old, cur)...)
		room := MaxModuleDiff - diff.Len()
		d, full, err := gitDiff(ctx, old, cur, room)
		switch {
		case err != nil:
			md.Unchecked = append(md.Unchecked, fmt.Sprintf("%s: diff: %v", b.Path, err))
		case !full:
			md.Unchecked = append(md.Unchecked, fmt.Sprintf("%s %s..%s: upstream diff exceeds the %d KiB the verifier reads", b.Path, b.From, b.To, MaxModuleDiff>>10))
		default:
			fmt.Fprintf(&diff, "### %s %s..%s\n\n%s\n", b.Path, b.From, b.To, d)
		}
	}
	md.Diff = diff.String()
	return md
}

// download fetches every version in one go command and returns the
// extracted directories, keyed path@version. It runs outside any module so
// the checkout's own go.mod, replace lines and GOFLAGS cannot steer it.
func download(ctx context.Context, bumps []Bump) (map[string]string, error) {
	tmp, err := os.MkdirTemp("", "marshal-moddl-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	args := []string{"mod", "download", "-json"}
	for _, b := range bumps {
		args = append(args, b.Path+"@"+b.From, b.Path+"@"+b.To)
	}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = tmp
	cmd.Env = append(contract.CleanEnv(), "GOFLAGS=", "GOTOOLCHAIN=local", "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &limited{buf: &stderr, n: 4 << 10}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	dirs := map[string]string{}
	dec := json.NewDecoder(io.LimitReader(out, 1<<20))
	var failed []string
	for {
		var m struct{ Path, Version, Dir, Error string }
		if dec.Decode(&m) != nil {
			break
		}
		if m.Error != "" {
			failed = append(failed, clip(m.Error, 200))
			continue
		}
		if filepath.IsAbs(m.Dir) {
			dirs[m.Path+"@"+m.Version] = m.Dir
		}
	}
	_, _ = io.Copy(io.Discard, out)
	werr := cmd.Wait()
	if len(failed) > 0 {
		return dirs, fmt.Errorf("%s", strings.Join(failed, "; "))
	}
	if werr != nil && len(dirs) == 0 {
		return nil, fmt.Errorf("%v: %s", werr, firstLine(stderr.String()))
	}
	return dirs, nil
}

// gitDiff diffs two extracted module trees, reading at most limit bytes.
// full is false when the diff was longer. Both trees sit side by side in
// the module cache, so running from their parent keeps the paths short.
// No user or system git config is read, so no external diff driver runs.
func gitDiff(ctx context.Context, old, cur string, limit int) (string, bool, error) {
	if limit <= 0 {
		return "", false, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a, b := old, cur
	dir := ""
	if filepath.Dir(old) == filepath.Dir(cur) {
		dir, a, b = filepath.Dir(old), filepath.Base(old), filepath.Base(cur)
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--no-index", "--no-color", "--no-ext-diff", "--no-textconv", "--", a, b)
	cmd.Dir = dir
	cmd.Env = append(contract.CleanEnv(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", false, err
	}
	if err := cmd.Start(); err != nil {
		return "", false, err
	}
	buf, _ := io.ReadAll(io.LimitReader(out, int64(limit)+1))
	full := len(buf) <= limit
	if !full {
		// Longer than we will read: stop git rather than drain it.
		cancel()
	}
	_, _ = io.Copy(io.Discard, out)
	werr := cmd.Wait()
	if full {
		// git diff --no-index exits 1 when the trees differ.
		if ee, ok := werr.(*exec.ExitError); werr != nil && !(ok && ee.ExitCode() == 1) {
			return "", false, werr
		}
		return string(buf), true, nil
	}
	return "", false, nil
}

// sensitiveImports are packages whose first appearance in a module is worth
// a human's eye: running programs, talking to the network, escaping the type
// system, or linking C.
var sensitiveImports = []string{"os/exec", "net", "net/http", "net/rpc", "unsafe", "C", "syscall", "plugin", "golang.org/x/sys/unix", "golang.org/x/sys/windows"}

// counted are constructs whose count rising between versions is a flag.
var counted = []struct {
	what string
	re   *regexp.Regexp
}{
	{"init functions", regexp.MustCompile(`(?m)^func init\(\)`)},
	{"//go:linkname directives", regexp.MustCompile(`(?m)^//go:linkname\b`)},
	{"process launches", regexp.MustCompile(`\bexec\.(Command|CommandContext)\(|\bos\.StartProcess\(|\bsyscall\.(Exec|ForkExec)\(`)},
	{"network calls", regexp.MustCompile(`\bhttp\.(Get|Post|PostForm|Head|NewRequest|NewRequestWithContext)\(|\bnet\.(Dial|DialTimeout|Listen|ListenPacket)\(|\.(Dial|DialContext)\(`)},
	{"file writes", regexp.MustCompile(`\bos\.(WriteFile|Create|OpenFile|Remove|RemoveAll|Rename|Chmod|Symlink|MkdirAll)\(|\bioutil\.WriteFile\(`)},
	{"cgo directives", regexp.MustCompile(`(?m)^\s*(//\s*)?#cgo\b`)},
}

// gained compares what the non-test Go source of two versions of a module
// can do, and reports what the new one can do that the old one could not.
// Counting, not diffing: a refactor that moves an exec call is quiet, a
// release that adds one is not.
func gained(b Bump, old, cur string) []string {
	o, n := scan(old), scan(cur)
	var out []string
	for _, imp := range sensitiveImports {
		if n.imports[imp] && !o.imports[imp] {
			out = append(out, fmt.Sprintf("%s %s imports %s, which %s did not", b.Path, b.To, strconv.Quote(imp), b.From))
		}
	}
	for _, c := range counted {
		if n.counts[c.what] > o.counts[c.what] {
			out = append(out, fmt.Sprintf("%s %s has %d %s, %s had %d", b.Path, b.To, n.counts[c.what], c.what, b.From, o.counts[c.what]))
		}
	}
	if n.other > o.other {
		out = append(out, fmt.Sprintf("%s %s ships %d non-Go source or binary files (assembly, C, objects), %s shipped %d", b.Path, b.To, n.other, b.From, o.other))
	}
	return out
}

type tally struct {
	imports map[string]bool
	counts  map[string]int
	other   int
}

func scan(dir string) tally {
	t := tally{imports: map[string]bool{}, counts: map[string]int{}}
	fset := token.NewFileSet()
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		switch filepath.Ext(name) {
		case ".s", ".S", ".c", ".cc", ".cpp", ".h", ".syso", ".o", ".a", ".so", ".dylib", ".dll":
			t.other++
			return nil
		case ".go":
		default:
			return nil
		}
		if strings.HasSuffix(name, "_test.go") || strings.Contains(path, string(filepath.Separator)+"testdata"+string(filepath.Separator)) {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > maxSourceFile {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if f, err := parser.ParseFile(fset, path, src, parser.ImportsOnly); err == nil {
			for _, imp := range f.Imports {
				if p, err := strconv.Unquote(imp.Path.Value); err == nil {
					t.imports[p] = true
				}
			}
		}
		for _, c := range counted {
			t.counts[c.what] += len(c.re.FindAllIndex(src, -1))
		}
		return nil
	})
	return t
}

// limited keeps the first n bytes written and discards the rest.
type limited struct {
	buf *bytes.Buffer
	n   int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.n - l.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		l.buf.Write(p[:room])
	}
	return len(p), nil
}
