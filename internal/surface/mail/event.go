package mail

import (
	"encoding/json"
	"errors"
	"fmt"
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

// ParseCodexNotify reads the JSON Codex passes as the last argument to its
// notify program. Only agent-turn-complete is a turn event. The thread id key
// is not pinned yet (spike S2), so the common spellings are all accepted.
func ParseCodexNotify(data []byte) (WorkerState, string, error) {
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		return "", "", fmt.Errorf("codex notify payload: %w", err)
	}
	typ, _ := p["type"].(string)
	if typ != "agent-turn-complete" {
		return "", "", fmt.Errorf("codex notify %s is %w", quoteTrunc(typ), ErrNotTurnEvent)
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
