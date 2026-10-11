package runner

import (
	"strings"
	"testing"
)

// recorded is a trimmed capture of a real headless run.
const recorded = `{"type":"system","subtype":"init","session_id":"abc","model":"claude-haiku-5-5"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Looking at the workflow.\n"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":".github/workflows/ci.yml"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"git push origin main"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Pin actions to SHAs\n\nPinned three actions."}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":4,"duration_ms":12345,"total_cost_usd":0.42,"session_id":"abc","permission_denials":[{"tool_name":"Bash","tool_input":{"command":"git push origin main"}}],"result":"Pin actions to SHAs\n\nPinned three actions."}
`

func TestParseStream(t *testing.T) {
	var got []Event
	res, err := ParseStream(strings.NewReader(recorded), func(e Event) { got = append(got, e) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Turns != 4 || res.CostUSD != 0.42 || res.SessionID != "abc" || res.IsError {
		t.Errorf("result summary wrong: %+v", res)
	}
	if res.Duration.Seconds() < 12 || res.Duration.Seconds() > 13 {
		t.Errorf("duration %v, want ~12.3s", res.Duration)
	}
	if !strings.HasPrefix(res.Output, "Pin actions to SHAs") {
		t.Errorf("output should be the last text block, got %q", res.Output)
	}
	if len(res.Denials) != 1 || res.Denials[0] != "Bash git push origin main" {
		t.Errorf("denials = %v", res.Denials)
	}
	kinds := map[Kind]int{}
	for _, e := range got {
		kinds[e.Kind]++
	}
	if kinds[Text] != 2 || kinds[Tool] != 2 || kinds[Denied] != 1 || kinds[Note] != 1 {
		t.Errorf("event kinds = %v", kinds)
	}
	for _, e := range got {
		if e.Kind == Tool && e.Tool == "Bash" && e.Text != "Bash git push origin main" {
			t.Errorf("summary = %q", e.Text)
		}
	}
}

func TestParseStreamNoResult(t *testing.T) {
	_, err := ParseStream(strings.NewReader(`{"type":"system","subtype":"init"}`+"\n"), func(Event) {})
	if err == nil {
		t.Fatal("want error when the stream ends without a result")
	}
}

func TestParseStreamStructured(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"structured_output":{"verdict":"PASS","reason":"ok"},"result":"{\"verdict\":\"PASS\"}"}` + "\n"
	res, err := ParseStream(strings.NewReader(line), func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Structured) != `{"verdict":"PASS","reason":"ok"}` {
		t.Errorf("structured = %s", res.Structured)
	}
	if a := strings.Join(Claude{}.Args(Request{ExtraDirs: []string{"/m/skills"}}), " "); !strings.Contains(a, "--add-dir /m/skills") {
		t.Errorf("extra dirs missing: %s", a)
	}
	args := Claude{}.Args(Request{JSONSchema: `{"type":"object"}`})
	if !strings.Contains(strings.Join(args, " "), `--json-schema {"type":"object"}`) {
		t.Errorf("schema flag missing: %v", args)
	}
	if strings.Contains(shortArgs(args), `"type"`) {
		t.Error("exec note should hide the schema body")
	}
}

func TestParseStreamError(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","result":"Not logged in","num_turns":1}` + "\n"
	res, err := ParseStream(strings.NewReader(line), func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Output != "Not logged in" || res.Stop != "api_error" {
		t.Errorf("%+v", res)
	}
}

func TestArgs(t *testing.T) {
	args := Claude{}.Args(Request{
		Model: "m", Effort: "high", MaxTurns: 30, MaxUSD: 2.5,
		PermissionMode: "acceptEdits",
		AllowedTools:   []string{"Read", "Bash(go:*)"},
		SystemPrompt:   "conventions",
	})
	s := strings.Join(args, " ")
	for _, want := range []string{"-p", "--output-format stream-json", "--model m", "--effort high",
		"--max-turns 30", "--max-budget-usd 2.50", "--permission-mode acceptEdits",
		"--allowedTools Read Bash(go:*)", "--append-system-prompt conventions", "--no-session-persistence",
		"--setting-sources project", "--strict-mcp-config"} {
		if !strings.Contains(s, want) {
			t.Errorf("args missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "--bare") {
		t.Error("--bare skips credential loading and must not be used")
	}
}

func TestSummarise(t *testing.T) {
	long := strings.Repeat("x", 200)
	s := Summarise("Bash", []byte(`{"command":"`+long+`"}`))
	if len(s) > 130 || !strings.HasSuffix(s, "...") {
		t.Errorf("long command not truncated: %d chars", len(s))
	}
	if Summarise("Weird", []byte(`{}`)) != "Weird" {
		t.Error("tool with no known argument should be its bare name")
	}
}

func TestArgsNeverFloor(t *testing.T) {
	// The floor reaches every request, including ones whose caller built
	// its own tool lists and never saw the fleet file.
	args := Claude{Never: []string{"WebFetch", "Bash(curl:*)"}}.Args(Request{
		AllowedTools:    []string{"Read", "WebFetch"},
		DisallowedTools: []string{"Bash(curl:*)"},
	})
	s := strings.Join(args, " ")
	if !strings.Contains(s, "--allowedTools Read --disallowedTools Bash(curl:*) WebFetch") {
		t.Errorf("floor not applied:\n%s", s)
	}
}
