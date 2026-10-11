package main

import (
	"os"
	"testing"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/jobs"
)

func TestStarterFleetParses(t *testing.T) {
	f, err := config.Parse([]byte(starterFleet))
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range f.Jobs {
		if _, err := f.PromptText(j, jobs.Prompt); err != nil {
			t.Errorf("starter job %q has no brief: %v", j.Name, err)
		}
	}
}

func TestStarterMatchesExample(t *testing.T) {
	b, err := os.ReadFile("examples/fleet.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != starterFleet {
		t.Error("examples/fleet.yaml differs from the starter in starter.go; keep them identical")
	}
}
