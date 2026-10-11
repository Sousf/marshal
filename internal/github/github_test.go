package github

import (
	"encoding/json"
	"testing"
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

func TestCIRed(t *testing.T) {
	if !(CI{Status: "completed", Conclusion: "failure"}).Red() {
		t.Error("failure is red")
	}
	if (CI{Status: "in_progress"}).Red() || (CI{Status: "completed", Conclusion: "success"}).Red() {
		t.Error("running or green is not red")
	}
}
