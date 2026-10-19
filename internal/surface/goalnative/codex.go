package goalnative

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

type codexPrompt struct {
	prompt   NativePrompt
	accepted chan struct{}
	done     chan struct{}
	once     sync.Once
	result   TurnResult
	err      error
}

type codexSession struct {
	native  *Session
	observe func(NativeEvent)
	mu      sync.Mutex
	active  *codexPrompt
	turns   map[string]*codexPrompt
}

func StartCodex(ctx context.Context, config Config, observe func(NativeEvent)) (NativeSession, error) {
	session := &codexSession{observe: observe, turns: make(map[string]*codexPrompt)}
	rawObserver := config.OnEvent
	config.OnEvent = func(raw json.RawMessage) {
		if rawObserver != nil {
			rawObserver(raw)
		}
		session.consume(raw)
	}
	config.OnPromptAccepted = session.accept
	native, err := Start(ctx, config)
	if err != nil {
		return nil, err
	}
	session.mu.Lock()
	session.native = native
	session.mu.Unlock()
	return session, nil
}

func (session *codexSession) Identity() NativeIdentity {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.native == nil {
		return NativeIdentity{}
	}
	return NativeIdentity{Harness: "codex", SessionID: session.native.ThreadID, Provider: session.native.Provider, Model: session.native.Model}
}

func (session *codexSession) Submit(ctx context.Context, prompt NativePrompt) error {
	if err := ValidatePrompt(prompt); err != nil {
		return err
	}
	turn := &codexPrompt{prompt: prompt, accepted: make(chan struct{}), done: make(chan struct{})}
	session.mu.Lock()
	if session.active != nil || session.turns[prompt.ID] != nil || len(session.turns) >= 256 {
		session.mu.Unlock()
		return errors.New("codex request overlaps, repeats or exceeds its session bound")
	}
	session.active = turn
	session.turns[prompt.ID] = turn
	session.mu.Unlock()
	go func() {
		result, err := session.native.Turn(ctx, prompt.Text)
		session.finish(turn, result, err)
	}()
	select {
	case <-turn.accepted:
		return nil
	case <-turn.done:
		select {
		case <-turn.accepted:
			return nil
		default:
		}
		session.mu.Lock()
		err := turn.err
		session.mu.Unlock()
		if err != nil {
			return err
		}
		return errors.New("codex completed without prompt acceptance evidence")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (session *codexSession) finish(turn *codexPrompt, result TurnResult, err error) {
	status := result.Status
	if err != nil {
		status = "failed"
	}
	session.emit(NativeEvent{Identity: session.Identity(), RequestID: turn.prompt.ID, NativeID: result.ID, Kind: "turn", Status: status, Text: result.Text, InputSHA256: PromptDigest(turn.prompt.Text), Evidence: "codex-turn-completed", Settled: err == nil})
	session.mu.Lock()
	defer session.mu.Unlock()
	turn.result, turn.err = result, err
	session.active = nil
	close(turn.done)
}

func (session *codexSession) accept(turnID, text string) {
	session.mu.Lock()
	turn := session.active
	if turn == nil || turn.prompt.Text != text {
		session.mu.Unlock()
		return
	}
	session.mu.Unlock()
	session.emit(NativeEvent{Identity: session.Identity(), RequestID: turn.prompt.ID, NativeID: turnID, Kind: "prompt", Status: "accepted", InputSHA256: PromptDigest(text), Evidence: "codex-turn-start-response"})
	turn.once.Do(func() { close(turn.accepted) })
}

func (session *codexSession) consume(raw json.RawMessage) {
	var event struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			Item     struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"item"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &event) != nil || event.Method != "item/completed" || event.Params.Item.Type != "userMessage" || event.Params.ThreadID != session.Identity().SessionID {
		return
	}
	session.mu.Lock()
	turn := session.active
	session.mu.Unlock()
	if turn == nil {
		return
	}
	for _, content := range event.Params.Item.Content {
		if content.Type == "text" && content.Text == turn.prompt.Text {
			session.emit(NativeEvent{Identity: session.Identity(), RequestID: turn.prompt.ID, NativeID: event.Params.TurnID, Kind: "prompt", Status: "consumed", InputSHA256: PromptDigest(content.Text), Evidence: "codex-user-message"})
		}
	}
}

func (session *codexSession) Wait(ctx context.Context, requestID string) (TurnResult, error) {
	session.mu.Lock()
	turn := session.turns[requestID]
	session.mu.Unlock()
	if turn == nil {
		return TurnResult{}, errors.New("unknown native request")
	}
	select {
	case <-turn.done:
		session.mu.Lock()
		defer session.mu.Unlock()
		return turn.result, turn.err
	case <-ctx.Done():
		return TurnResult{}, ctx.Err()
	}
}

func (session *codexSession) emit(event NativeEvent) {
	if session.observe != nil {
		session.observe(event)
	}
}

func (session *codexSession) Turn(ctx context.Context, text string) (TurnResult, error) {
	return RunTurn(ctx, session, text)
}

func (session *codexSession) WaitIdle(ctx context.Context) error {
	for {
		session.mu.Lock()
		active := session.active
		session.mu.Unlock()
		if active == nil {
			return session.native.WaitIdle(ctx)
		}
		select {
		case <-active.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (session *codexSession) Close(ctx context.Context) error { return session.native.Close(ctx) }
