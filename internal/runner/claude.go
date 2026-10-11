package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Claude runs Claude Code in headless mode and parses its stream-json output.
type Claude struct {
	// Binary overrides the executable name. Empty means "claude" on PATH.
	Binary string
}

// Name implements Adapter.
func (Claude) Name() string { return "claude-code" }

// Available implements Adapter.
func (c Claude) Available() bool {
	_, err := exec.LookPath(c.binary())
	return err == nil
}

func (c Claude) binary() string {
	if c.Binary != "" {
		return c.Binary
	}
	return "claude"
}

// Args builds the command line for a request. Exposed so a dry run can show
// exactly what would execute.
func (c Claude) Args(req Request) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--no-session-persistence",
		// Project settings only. Without this the agent inherits whatever
		// the host user keeps in ~/.claude: a personal CLAUDE.md, hooks,
		// MCP servers. Observed on the first real run, when the agent
		// followed the operator's private commit rules instead of the brief.
		// Credentials are not settings and still load.
		"--setting-sources", "project",
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	if req.MaxUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(req.MaxUSD, 'f', 2, 64))
	}
	if req.PermissionMode != "" {
		args = append(args, "--permission-mode", req.PermissionMode)
	}
	if len(req.AllowedTools) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, req.AllowedTools...)
	}
	if len(req.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools")
		args = append(args, req.DisallowedTools...)
	}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}
	if req.JSONSchema != "" {
		args = append(args, "--json-schema", req.JSONSchema)
	}
	return args
}

// Run implements Adapter. The prompt goes in on stdin so a long brief never
// meets an argument-length limit.
func (c Claude) Run(ctx context.Context, req Request, emit func(Event)) (Result, error) {
	args := c.Args(req)
	cmd := exec.CommandContext(ctx, c.binary(), args...)
	cmd.Dir = req.Dir
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Env = append(os.Environ(), req.Env...)
	// Stderr carries MCP connection chatter and the occasional real error.
	// Keep the last few lines for the failure message rather than dropping
	// them, which is what the first version of this did and regretted.
	var stderr tail
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	emit(Event{At: time.Now(), Kind: Note, Text: "exec " + c.binary() + " " + shortArgs(args)})
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start %s: %w", c.binary(), err)
	}

	res, perr := ParseStream(stdout, emit)
	werr := cmd.Wait()
	if res.Duration == 0 {
		res.Duration = time.Since(started)
	}
	switch {
	case ctx.Err() != nil:
		return res, ctx.Err()
	case perr != nil:
		return res, perr
	case werr != nil:
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = res.Output
		}
		return res, fmt.Errorf("%s: %w: %s", c.binary(), werr, msg)
	}
	return res, nil
}

// event is the subset of stream-json MARSHAL reads.
type event struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
	// result fields
	Result            string          `json:"result"`
	IsError           bool            `json:"is_error"`
	CostUSD           float64         `json:"total_cost_usd"`
	NumTurns          int             `json:"num_turns"`
	DurationMS        int64           `json:"duration_ms"`
	SessionID         string          `json:"session_id"`
	TerminalReason    string          `json:"terminal_reason"`
	Structured        json.RawMessage `json:"structured_output"`
	PermissionDenials []struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	} `json:"permission_denials"`
}

// ParseStream reads stream-json lines, emitting events as they arrive, and
// returns the summary from the final result line. It is separate from Run so
// it can be tested against recorded output without an agent present.
func ParseStream(r io.Reader, emit func(Event)) (Result, error) {
	var res Result
	sc := bufio.NewScanner(r)
	// A single assistant turn carrying a long report exceeds bufio's 64 KB
	// default. A truncated report is worse than a slow one.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	sawResult := false
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		now := time.Now()
		switch e.Type {
		case "assistant":
			for _, c := range e.Message.Content {
				switch c.Type {
				case "text":
					t := strings.TrimRight(c.Text, "\n")
					if t == "" {
						continue
					}
					res.Output = t
					emit(Event{At: now, Kind: Text, Text: t})
				case "tool_use":
					emit(Event{At: now, Kind: Tool, Tool: c.Name, Input: c.Input, Text: Summarise(c.Name, c.Input)})
				}
			}
		case "result":
			sawResult = true
			res.CostUSD = e.CostUSD
			res.Turns = e.NumTurns
			res.Duration = time.Duration(e.DurationMS) * time.Millisecond
			res.SessionID = e.SessionID
			res.IsError = e.IsError
			res.Stop = e.TerminalReason
			if e.Subtype != "" && e.Subtype != "success" {
				res.Stop = e.Subtype
			}
			if e.IsError && e.Result != "" {
				res.Output = e.Result
			}
			if len(e.Structured) > 0 && string(e.Structured) != "null" {
				res.Structured = e.Structured
			}
			for _, d := range e.PermissionDenials {
				what := Summarise(d.ToolName, d.ToolInput)
				res.Denials = append(res.Denials, what)
				emit(Event{At: now, Kind: Denied, Tool: d.ToolName, Input: d.ToolInput, Text: "denied " + what})
			}
			emit(Event{At: now, Kind: Note, Text: fmt.Sprintf(
				"%d turns · %.0fs · $%.2f · %s", e.NumTurns,
				float64(e.DurationMS)/1000, e.CostUSD, stopWord(res))})
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	if !sawResult {
		return res, fmt.Errorf("stream ended without a result event")
	}
	return res, nil
}

func stopWord(r Result) string {
	switch {
	case r.IsError && r.Stop != "":
		return "error: " + r.Stop
	case r.IsError:
		return "error"
	case r.Stop != "" && r.Stop != "success":
		return r.Stop
	}
	return "done"
}

// Summarise renders a tool call as one line for the activity log.
func Summarise(name string, input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := in[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	var arg string
	switch name {
	case "Bash":
		arg = pick("command")
	case "Read", "Write", "Edit", "NotebookEdit":
		arg = pick("file_path", "path")
	case "Grep", "Glob":
		arg = pick("pattern")
	case "WebFetch":
		arg = pick("url")
	case "WebSearch":
		arg = pick("query")
	case "Task":
		arg = pick("description")
	default:
		arg = pick("command", "file_path", "pattern", "url", "query", "description")
	}
	arg = strings.ReplaceAll(arg, "\n", " ")
	if len(arg) > 120 {
		arg = arg[:117] + "..."
	}
	if arg == "" {
		return name
	}
	return name + " " + arg
}

// shortArgs hides the system prompt, which is long and already on record,
// from the exec note.
func shortArgs(args []string) string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if (args[i] == "--append-system-prompt" || args[i] == "--json-schema") && i+1 < len(args) {
			out = append(out, args[i], fmt.Sprintf("<%d bytes>", len(args[i+1])))
			i++
			continue
		}
		out = append(out, args[i])
	}
	return strings.Join(out, " ")
}

// tail keeps the last few KB written to it.
type tail struct{ buf []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string { return string(t.buf) }
