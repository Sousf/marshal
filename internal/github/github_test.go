package github

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:Sousf/spotify-tui.git":     "Sousf/spotify-tui",
		"https://github.com/Sousf/marshal":         "Sousf/marshal",
		"https://github.com/Sousf/marshal.git":     "Sousf/marshal",
		"https://github.com/Sousf/marshal/":        "Sousf/marshal",
		"https://github.com/Sousf/marshal/pull/12": "Sousf/marshal",
	} {
		got, ok := Slug(in)
		if !ok || got != want {
			t.Errorf("Slug(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"/home/pete/dev/x", "git@gitlab.com:a/b.git", ""} {
		if _, ok := Slug(in); ok {
			t.Errorf("Slug(%q) should not match", in)
		}
	}
}

func TestRollup(t *testing.T) {
	cases := map[string]string{
		`[]`: "",
		`[{"conclusion":"SUCCESS"},{"conclusion":"SKIPPED"}]`:   "SUCCESS",
		`[{"conclusion":"SUCCESS"},{"status":"IN_PROGRESS"}]`:   "PENDING",
		`[{"conclusion":"SUCCESS"},{"conclusion":"FAILURE"}]`:   "FAILURE",
		`[{"state":"SUCCESS"},{"state":"PENDING"}]`:             "PENDING",
		`[{"conclusion":"TIMED_OUT"},{"status":"IN_PROGRESS"}]`: "FAILURE",
	}
	for in, want := range cases {
		var j prJSON
		if err := json.Unmarshal([]byte(`{"statusCheckRollup":`+in+`}`), &j); err != nil {
			t.Fatal(err)
		}
		if got := rollup(j); got != want {
			t.Errorf("rollup(%s) = %q want %q", in, got, want)
		}
	}
}

func TestFilterComments(t *testing.T) {
	var pc prCommentsJSON
	json.Unmarshal([]byte(`{"comments":[
		{"author":{"login":"pete"},"body":"please pin this","createdAt":"2026-10-11T10:00:00Z"},
		{"author":{"login":"marshal-sousf[bot]"},"body":"MARSHAL pushed a fix","createdAt":"2026-10-11T10:05:00Z"},
		{"author":{"login":"pete"},"body":"old","createdAt":"2026-10-10T10:00:00Z"}],
		"reviews":[{"author":{"login":"pete"},"body":"needs tests","state":"CHANGES_REQUESTED","submittedAt":"2026-10-11T09:00:00Z"},
		{"author":{"login":"pete"},"body":"","state":"COMMENTED","submittedAt":"2026-10-11T09:30:00Z"}]}`), &pc)
	inline := []inlineJSON{{Body: "rename this", Path: "main.go", CreatedAt: time.Date(2026, 10, 11, 9, 45, 0, 0, time.UTC)}}
	inline[0].User.Login = "pete"
	since := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	got := filterComments(pc, inline, "marshal-sousf[bot]", since)
	if len(got) != 3 {
		t.Fatalf("got %d comments: %+v", len(got), got)
	}
	if got[0].Body != "needs tests" || got[1].Where != "main.go" || got[2].Body != "please pin this" {
		t.Errorf("order or content wrong: %+v", got)
	}
	r := RenderComments(got)
	if !strings.Contains(r, "pete (review changes_requested") || !strings.Contains(r, "main.go") {
		t.Errorf("render:\n%s", r)
	}
}

func TestCIRed(t *testing.T) {
	if !(CI{Status: "completed", Conclusion: "failure"}).Red() {
		t.Error("failure is red")
	}
	if (CI{Status: "in_progress"}).Red() || (CI{Status: "completed", Conclusion: "success"}).Red() {
		t.Error("running or green is not red")
	}
}
