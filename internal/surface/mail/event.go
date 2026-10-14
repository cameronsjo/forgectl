package mail

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrNotTurnEvent marks a hook or notify payload that is well formed but is
// not a turn boundary. A hook fires for more than turns, so callers ignore it.
var ErrNotTurnEvent = errors.New("not a turn event")

// ParseClaudeHook reads the JSON a Claude Code hook gets on stdin. Stop ends a
// turn; UserPromptSubmit starts one. Anything else is not a turn event.
func ParseClaudeHook(data []byte) (WorkerState, error) {
	var p struct {
		Event string `json:"hook_event_name"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return "", fmt.Errorf("claude hook payload: %w", err)
	}
	switch p.Event {
	case "Stop":
		return StateIdle, nil
	case "UserPromptSubmit":
		return StateBusy, nil
	}
	return "", fmt.Errorf("claude hook %s is %w", quoteTrunc(p.Event), ErrNotTurnEvent)
}

// codexTitlePrompt opens the first input message of the internal thread
// Codex starts to name a session. That thread sends its own
// agent-turn-complete with the worker's cwd and client (spike S2, codex-cli
// 0.160.0), so it is not the worker's turn.
const codexTitlePrompt = "Generate a concise, single-line task title"

// ParseCodexNotify reads the JSON Codex passes as the last argument to its
// notify program. Only agent-turn-complete is a turn event, and not the one
// from Codex's title-generation thread. The thread id key is `thread-id`
// (spike S2); the other spellings stay accepted.
func ParseCodexNotify(data []byte) (WorkerState, string, error) {
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		return "", "", fmt.Errorf("codex notify payload: %w", err)
	}
	typ, _ := p["type"].(string)
	if typ != "agent-turn-complete" {
		return "", "", fmt.Errorf("codex notify %s is %w", quoteTrunc(typ), ErrNotTurnEvent)
	}
	if inputs, _ := p["input-messages"].([]any); len(inputs) > 0 {
		if first, _ := inputs[0].(string); strings.HasPrefix(strings.TrimSpace(first), codexTitlePrompt) {
			return "", "", fmt.Errorf("codex title-generation notify is %w", ErrNotTurnEvent)
		}
	}
	for _, k := range []string{"thread-id", "thread_id", "threadId"} {
		if v, ok := p[k].(string); ok && v != "" {
			return StateIdle, v, nil
		}
	}
	return StateIdle, "", nil
}

// ParseState maps an explicit --state flag, which the pi extension passes.
func ParseState(s string) (WorkerState, error) {
	switch WorkerState(s) {
	case StateIdle:
		return StateIdle, nil
	case StateBusy:
		return StateBusy, nil
	case StateWaiting:
		return StateWaiting, nil
	}
	return "", fmt.Errorf("state %s: want idle, busy or waiting", quoteTrunc(s))
}
