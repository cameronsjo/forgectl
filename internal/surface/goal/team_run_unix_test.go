//go:build unix

package goal

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/goalnative"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

type fakeNativeSession struct {
	identity goalnative.NativeIdentity
	closed   bool
}

func TestExplicitTeamContractUsesBrokerToolsForEveryHarness(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	engine := newNativeTeamTestExecution(fixture)
	contract := engine.contract()
	for _, required := range []string{"Every role, including Codex, Pi and Claude", "goal_read/goal_write/goal_exec/goal_mail/goal_inbox", "goal_mail queues {to,body}", "never call the surface CLI through goal_exec", "preliminary live source/diff review", "result.patch does not exist yet", "separate direct native review", "exact frozen patch digest", "Mail never constitutes final approval"} {
		if !strings.Contains(contract, required) {
			t.Errorf("explicit team contract omitted %q", required)
		}
	}
	for _, obsolete := range []string{"native apply_patch", "surface send", "surface inbox", "Queued/exit75"} {
		if strings.Contains(contract, obsolete) {
			t.Errorf("explicit team contract retained unavailable native command %q", obsolete)
		}
	}
	legacy := newGoalTestExecution(fixture).contract()
	for _, required := range []string{"native apply_patch", "surface send", "surface inbox"} {
		if !strings.Contains(legacy, required) {
			t.Errorf("legacy contract lost %q", required)
		}
	}
}

func (session *fakeNativeSession) Identity() goalnative.NativeIdentity { return session.identity }
func (session *fakeNativeSession) Submit(context.Context, goalnative.NativePrompt) error {
	return nil
}
func (session *fakeNativeSession) Wait(context.Context, string) (goalnative.TurnResult, error) {
	return goalnative.TurnResult{}, nil
}
func (session *fakeNativeSession) Turn(context.Context, string) (goalnative.TurnResult, error) {
	return goalnative.TurnResult{}, nil
}
func (session *fakeNativeSession) WaitIdle(context.Context) error { return nil }
func (session *fakeNativeSession) Close(context.Context) error {
	session.closed = true
	return nil
}

func normalizedIdentity() goalnative.NativeIdentity {
	return goalnative.NativeIdentity{Harness: "pi", SessionID: "native-session", Provider: "openai", Model: "gpt-5"}
}

func newNativeTeamTestExecution(fixture goalFixture) *execution {
	team := fixture.state.Spec.EffectiveTeam()
	fixture.state.Spec.Team = &team
	return newGoalTestExecution(fixture, Role{
		Name:            "coordinator",
		NativeSessionID: "native-session",
		Provider:        "openai",
		Model:           "gpt-5",
		Harness:         "pi",
	})
}

func takeFailure(t testing.TB, engine *execution) error {
	t.Helper()
	select {
	case err := <-engine.failure:
		return err
	default:
		return nil
	}
}

func registerTestPrompt(t testing.TB, engine *execution, id, text string) {
	t.Helper()
	if err := engine.registerNativePrompt("coordinator", goalnative.NativePrompt{ID: id, Text: text}); err != nil {
		t.Fatalf("register native prompt: %v", err)
	}
}

func TestNormalizedEventsRejectStaleAndFakeNativeIdentity(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	for _, testCase := range []struct {
		name     string
		identity goalnative.NativeIdentity
	}{
		{name: "stale session", identity: goalnative.NativeIdentity{Harness: "pi", SessionID: "old-session", Provider: "openai", Model: "gpt-5"}},
		{name: "fake harness", identity: goalnative.NativeIdentity{Harness: "codex", SessionID: "native-session", Provider: "openai", Model: "gpt-5"}},
		{name: "fake provider", identity: goalnative.NativeIdentity{Harness: "pi", SessionID: "native-session", Provider: "anthropic", Model: "gpt-5"}},
		{name: "fake model", identity: goalnative.NativeIdentity{Harness: "pi", SessionID: "native-session", Provider: "openai", Model: "other-model"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			engine := newNativeTeamTestExecution(fixture)
			engine.normalizedEvent("coordinator", goalnative.NativeEvent{
				Identity:  testCase.identity,
				RequestID: "request-stale",
				Kind:      "turn",
				Status:    "accepted",
			})
			if err := takeFailure(t, engine); err == nil || !strings.Contains(err.Error(), "did not match the bound role identity") {
				t.Fatalf("failure = %v, want identity refusal", err)
			}
			if len(engine.traces) != 0 {
				t.Fatalf("stale identity mutated traces: %#v", engine.traces)
			}
		})
	}
}

func TestNormalizedCompletionRequiresConsumedDigestAndSettledEvidence(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	message := mail.Message{V: 1, ID: "message-1", From: "reviewer", To: "coordinator", Body: "REVIEW_PASS " + fixture.state.ID + " patch"}
	digest := goalnative.PromptDigest(mail.Render(message))

	t.Run("accepted completion is not consumed", func(t *testing.T) {
		engine := newNativeTeamTestExecution(fixture)
		identity := normalizedIdentity()
		requestID := strings.Repeat("a", 32)
		registerTestPrompt(t, engine, requestID, "accepted-only")
		engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, Status: "accepted", Kind: "turn"})
		engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, NativeID: "native-accepted-only", Status: "completed", Text: "done", Settled: true, Kind: "turn"})
		if err := takeFailure(t, engine); err != nil {
			t.Fatal(err)
		}
		if engine.consumed("coordinator", message) {
			t.Fatal("accepted and settled completion counted without consumed input digest")
		}
	})

	t.Run("consumed wrong digest is not consumed", func(t *testing.T) {
		engine := newNativeTeamTestExecution(fixture)
		identity := normalizedIdentity()
		requestID := strings.Repeat("b", 32)
		registerTestPrompt(t, engine, requestID, "wrong-digest")
		engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, Status: "accepted", Kind: "turn"})
		engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, InputSHA256: strings.Repeat("0", 64), Status: "consumed", Kind: "turn"})
		if err := takeFailure(t, engine); err == nil || !strings.Contains(err.Error(), "input digest") {
			t.Fatalf("wrong digest failure = %v, want digest refusal", err)
		}
		if engine.consumed("coordinator", message) {
			t.Fatal("wrong consumed digest counted as the mailbox message")
		}
	})

	t.Run("consumed digest and settled completion are required", func(t *testing.T) {
		engine := newNativeTeamTestExecution(fixture)
		identity := normalizedIdentity()
		requestID := strings.Repeat("c", 32)
		registerTestPrompt(t, engine, requestID, mail.Render(message))
		engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, Status: "accepted", Kind: "turn"})
		engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, InputSHA256: digest, Status: "consumed", Kind: "turn"})
		engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, NativeID: "native-complete", Status: "completed", Text: "done", Settled: true, Kind: "turn"})
		if err := takeFailure(t, engine); err != nil {
			t.Fatal(err)
		}
		if !engine.consumed("coordinator", message) {
			t.Fatal("matching consumed digest and settled completion were not accepted")
		}
	})

	t.Run("unsettled completion cannot complete", func(t *testing.T) {
		engine := newNativeTeamTestExecution(fixture)
		identity := normalizedIdentity()
		requestID := strings.Repeat("d", 32)
		registerTestPrompt(t, engine, requestID, mail.Render(message))
		engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, Status: "accepted", Kind: "turn"})
		engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, InputSHA256: digest, Status: "consumed", Kind: "turn"})
		engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, NativeID: "native-unsettled", Status: "completed", Text: "done", Settled: false, Kind: "turn"})
		if err := takeFailure(t, engine); err == nil || !strings.Contains(err.Error(), "lacks settled identity") {
			t.Fatalf("failure = %v, want unsettled completion refusal", err)
		}
		if engine.consumed("coordinator", message) {
			t.Fatal("unsettled completion counted as consumed")
		}
	})
}

func TestNormalizedEventsPreserveTerminalStateAndDeduplicateConsumedInput(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	engine := newNativeTeamTestExecution(fixture)
	identity := normalizedIdentity()
	requestID := strings.Repeat("e", 32)
	registerTestPrompt(t, engine, requestID, "terminal-order")
	digest := goalnative.PromptDigest("terminal-order")
	accepted := goalnative.NativeEvent{Identity: identity, RequestID: requestID, Status: "accepted", Kind: "turn"}
	consumed := goalnative.NativeEvent{Identity: identity, RequestID: requestID, InputSHA256: digest, Status: "consumed", Kind: "turn"}
	completed := goalnative.NativeEvent{Identity: identity, RequestID: requestID, NativeID: "native-terminal", Status: "completed", Text: "done", Settled: true, Kind: "turn"}
	engine.normalizedEvent("coordinator", accepted)
	engine.normalizedEvent("coordinator", consumed)
	engine.normalizedEvent("coordinator", consumed)
	if err := takeFailure(t, engine); err != nil {
		t.Fatalf("duplicate consumed event failed: %v", err)
	}
	engine.normalizedEvent("coordinator", completed)
	if got := len(engine.traces["coordinator"].Inputs); got != 1 {
		t.Fatalf("consumed inputs = %d, want one after duplicate", got)
	}
	engine.normalizedEvent("coordinator", completed)
	if err := takeFailure(t, engine); err != nil {
		t.Fatalf("identical terminal duplicate failed: %v", err)
	}
	engine.normalizedEvent("coordinator", accepted)
	if err := takeFailure(t, engine); err == nil || !strings.Contains(err.Error(), "reopened a terminal") {
		t.Fatalf("late accepted event failure = %v, want terminal refusal", err)
	}
	turn := engine.traces["coordinator"].Turns[requestID]
	if turn.Status != "completed" || turn.Text != "done" {
		t.Fatalf("late event mutated terminal turn: %#v", turn)
	}
}

func TestTrackedNativeSessionRegistersPromptBeforeSubmit(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	engine := newNativeTeamTestExecution(fixture)
	session := &trackedNativeSession{role: "coordinator", engine: engine, native: &fakeNativeSession{identity: normalizedIdentity()}}
	prompt, err := goalnative.NewPrompt("pre-register this prompt")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Submit(context.Background(), prompt); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	trace := engine.traces["coordinator"]
	if trace == nil || trace.Expected[prompt.ID] != goalnative.PromptDigest(prompt.Text) {
		t.Fatalf("registered prompt = %#v, want digest", trace)
	}
}

func TestNormalizedEventsAllowConsumedBeforeAccepted(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	engine := newNativeTeamTestExecution(fixture)
	identity := normalizedIdentity()
	requestID := strings.Repeat("f", 32)
	registerTestPrompt(t, engine, requestID, "consumed first")
	digest := goalnative.PromptDigest("consumed first")
	engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, InputSHA256: digest, Status: "consumed", Kind: "turn"})
	engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, Status: "accepted", Kind: "turn"})
	engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, NativeID: "native-consumed-first", Status: "completed", Text: "done", Settled: true, Kind: "turn"})
	if err := takeFailure(t, engine); err != nil {
		t.Fatalf("consumed-before-accepted ordering failed: %v", err)
	}
	trace := engine.traces["coordinator"]
	if len(trace.Inputs) != 1 || trace.Turns[requestID].Status != "completed" {
		t.Fatalf("trace after reordered events = %#v, want one input and completed turn", trace)
	}
	engine.normalizedEvent("coordinator", goalnative.NativeEvent{Identity: identity, RequestID: requestID, InputSHA256: digest, Status: "consumed", Kind: "turn"})
	if err := takeFailure(t, engine); err != nil {
		t.Fatalf("duplicate consumed event failed: %v", err)
	}
	if got := len(trace.Inputs); got != 1 {
		t.Fatalf("inputs after duplicate consumed = %d, want one", got)
	}
}

func TestNativeMailboxRejectsWrongRunSessionAndHarness(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	session := &fakeNativeSession{identity: normalizedIdentity()}
	engine := newNativeTeamTestExecution(fixture)
	engine.roles = map[string]*nativeRole{
		"coordinator": {
			Name:    "coordinator",
			Session: session,
			Member:  mail.Worker{Name: "coordinator", Harness: mail.HarnessPi, RunID: "run-current", ThreadID: "native-session"},
		},
	}
	adapter := &nativeMailboxAdapter{engine: engine, context: context.Background()}
	base := engine.roles["coordinator"].Member
	for _, testCase := range []struct {
		name   string
		worker mail.Worker
		want   string
	}{
		{name: "wrong run", worker: mail.Worker{Name: base.Name, Harness: base.Harness, RunID: "run-old", ThreadID: base.ThreadID}, want: "incarnation"},
		{name: "wrong session", worker: mail.Worker{Name: base.Name, Harness: base.Harness, RunID: base.RunID, ThreadID: "native-old"}, want: "incarnation"},
		{name: "wrong harness", worker: mail.Worker{Name: base.Name, Harness: mail.HarnessClaude, RunID: base.RunID, ThreadID: base.ThreadID}, want: "incarnation"},
		{name: "unknown role", worker: mail.Worker{Name: "reviewer", Harness: mail.HarnessPi, RunID: base.RunID, ThreadID: base.ThreadID}, want: "incarnation"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := adapter.member(testCase.worker); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("member(%#v) = %v, want rejection", testCase.worker, err)
			}
		})
	}
	if got, err := adapter.member(base); err != nil || got != session {
		t.Fatalf("valid member = %v, %v; want bound session", got, err)
	}
	if session.closed {
		t.Fatal("mailbox identity check closed the native session")
	}
}

func TestNativeMailboxRejectsClosedRole(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	engine := newNativeTeamTestExecution(fixture)
	engine.roles = map[string]*nativeRole{"coordinator": {Name: "coordinator", Closed: true, Member: mail.Worker{Name: "coordinator", Harness: mail.HarnessPi, RunID: "run-current", ThreadID: "native-session"}}}
	adapter := &nativeMailboxAdapter{engine: engine, context: context.Background()}
	_, err := adapter.member(engine.roles["coordinator"].Member)
	if err == nil || !strings.Contains(err.Error(), "incarnation") {
		t.Fatalf("closed member = %v, want closed-session refusal", err)
	}
}

func TestNativeMailboxRequiresNativeSessionImplementation(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	engine := newNativeTeamTestExecution(fixture)
	engine.roles = map[string]*nativeRole{"coordinator": {Name: "coordinator", Member: mail.Worker{Name: "coordinator", Harness: mail.HarnessPi, RunID: "run-current", ThreadID: "native-session"}}}
	adapter := &nativeMailboxAdapter{engine: engine, context: context.Background()}
	_, err := adapter.member(engine.roles["coordinator"].Member)
	if err == nil || !strings.Contains(err.Error(), "session is not available") {
		t.Fatalf("missing session = %v, want unavailable refusal", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal("identity refusal unexpectedly reported context cancellation")
	}
}
