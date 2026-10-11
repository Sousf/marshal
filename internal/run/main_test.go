package run

import (
	"os"
	"testing"
)

// TestMain runs the package as CI does: no global or system git config,
// so a test that only passes thanks to the developer's own identity fails
// here too. Commits made by tests get a fixed, neutral identity.
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "test",
		"GIT_AUTHOR_EMAIL":    "test@example.invalid",
		"GIT_COMMITTER_NAME":  "test",
		"GIT_COMMITTER_EMAIL": "test@example.invalid",
	} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}
