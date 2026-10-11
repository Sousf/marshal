// Package runner drives a coding agent against a checked-out repository and
// reports what it does as a stream of events.
//
// The Adapter interface is the seam that keeps MARSHAL agent-agnostic. Claude
// Code headless is the first implementation; an Agent Client Protocol adapter
// is the planned second. Everything above this package sees only Events.
package runner

import (
	"context"
	"encoding/json"
	"time"
)

// Kind classifies an Event.
type Kind string

const (
	// Text is prose from the agent. The last Text of a run is its report.
	Text Kind = "text"
	// Tool is a tool call the agent made.
	Tool Kind = "tool"
	// Denied is a tool call the harness refused.
	Denied Kind = "denied"
	// Note is something the adapter wants on the record: the command line it
	// ran, a stderr line, a budget stop.
	Note Kind = "note"
)

// Event is one thing that happened during a run.
type Event struct {
	At    time.Time
	Kind  Kind
	Text  string
	Tool  string
	Input json.RawMessage
}

// Request is everything an adapter needs to start a run.
type Request struct {
	// Prompt is the brief, sent as the user turn.
	Prompt string
	// SystemPrompt is appended to the agent's own system prompt. Repo
	// conventions from AGENTS.md go here so they outrank the brief.
	SystemPrompt string
	// Dir is the working tree the agent operates in.
	Dir string

	Model          string
	Effort         string
	MaxTurns       int
	MaxUSD         float64
	PermissionMode string
	// AllowedTools and DisallowedTools use the harness's own pattern syntax,
	// for example "Bash(go:*)". Disallowed wins on conflict.
	AllowedTools    []string
	DisallowedTools []string
	// Env is added to the agent's environment. Secrets for one run go here
	// rather than in the daemon's environment.
	Env []string
	// JSONSchema, when set, asks the harness for a structured final answer
	// matching the schema. The parsed object lands in Result.Structured.
	JSONSchema string
}

// Result summarises a finished run.
type Result struct {
	// Output is the agent's final text, which becomes the PR body.
	Output    string
	CostUSD   float64
	Turns     int
	Duration  time.Duration
	SessionID string
	// IsError is set when the harness itself reported failure, as opposed to
	// the agent finishing with nothing to do.
	IsError bool
	// Denials lists tool calls the harness refused, summarised. A run with
	// denials did not do everything it tried to; the reviewer should know.
	Denials []string
	// Stop is the harness's own reason for ending, when it gives one.
	Stop string
	// Structured is the schema-shaped answer when one was requested.
	Structured json.RawMessage
}

// Adapter runs an agent.
type Adapter interface {
	// Name identifies the adapter in run records.
	Name() string
	// Available reports whether the adapter can run on this host.
	Available() bool
	// Run executes one request, calling emit for each event as it happens.
	// emit is called from a single goroutine and must not block for long.
	Run(ctx context.Context, req Request, emit func(Event)) (Result, error)
}
