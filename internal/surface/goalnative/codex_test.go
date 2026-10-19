package goalnative

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCodexCompletionPublishesBeforeWaitIdleAndNextSubmit(t *testing.T) {
	prompt := NativePrompt{ID: strings.Repeat("d", 32), Text: "probe"}
	turn := &codexPrompt{prompt: prompt, accepted: make(chan struct{}), done: make(chan struct{})}
	nativeIdle := make(chan struct{})
	close(nativeIdle)
	session := &codexSession{native: &Session{ThreadID: "thread", turnIdle: nativeIdle}, active: turn, turns: map[string]*codexPrompt{prompt.ID: turn}}
	entered, release := make(chan struct{}), make(chan struct{})
	session.observe = func(event NativeEvent) {
		_ = session.Identity()
		if event.Status != "completed" || event.RequestID != prompt.ID {
			t.Errorf("unexpected event: %#v", event)
		}
		close(entered)
		<-release
	}
	finished := make(chan struct{})
	go func() {
		session.finish(turn, TurnResult{ID: "native-turn", Status: "completed", Text: "done"}, nil)
		close(finished)
	}()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := session.Wait(ctx, prompt.ID); !errors.Is(err, context.Canceled) {
		t.Errorf("unpublished Wait = %v", err)
	}
	if err := session.WaitIdle(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("unpublished WaitIdle = %v", err)
	}
	if err := session.Submit(t.Context(), NativePrompt{ID: strings.Repeat("e", 32), Text: "next"}); err == nil {
		t.Error("next prompt started before completion publication")
	}
	close(release)
	<-finished
	if result, err := session.Wait(t.Context(), prompt.ID); err != nil || result.Text != "done" {
		t.Errorf("published Wait = %#v, %v", result, err)
	}
	if err := session.WaitIdle(t.Context()); err != nil {
		t.Errorf("published WaitIdle = %v", err)
	}
}
