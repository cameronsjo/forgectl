//go:build unix

package goal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/goalnative"
	"github.com/cameronsjo/forgectl/internal/surface/goaltools"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/surface/project"
)

func (engine *execution) startNativeTeam(ctx context.Context) error {
	state := engine.snapshot()
	_, created, err := (project.Store{Root: engine.store.Root}).CreateProject(ctx, state.Spec.Title, state.Worktree, state.Spec.Objective)
	if err != nil {
		return err
	}
	ledger := filepath.Join(engine.store.Root, created.ID, "team")
	if err := os.MkdirAll(ledger, 0o700); err != nil {
		return err
	}
	if err := engine.update(func(state *State) { state.ProjectID, state.Ledger = created.ID, ledger }); err != nil {
		return err
	}
	socketRoot, err := os.MkdirTemp("/tmp", "fg-team-")
	if err != nil {
		return err
	}
	engine.socketRoot = socketRoot
	engine.socketRoot, err = filepath.EvalSymlinks(socketRoot)
	if err != nil {
		return err
	}
	if err := engine.record(Event{Time: time.Now().UTC(), Kind: "socket-root", Summary: engine.socketRoot}); err != nil {
		return err
	}
	threads := map[string]string{"coordinator": created.CoordinatorThreadID}
	for _, name := range []string{"implementation", "reviewer"} {
		_, thread, err := (project.Store{Root: engine.store.Root}).CreateThread(ctx, created.ID, name, state.Spec.Objective)
		if err != nil {
			return err
		}
		threads[name] = thread.ID
	}
	allRoots := []string{state.Worktree}
	team := state.Spec.EffectiveTeam()
	for _, name := range []string{"coordinator", "implementation", "reviewer"} {
		directory := filepath.Join(state.RunDir, "agents", name)
		for _, suffix := range []string{"", "tmp", "cache"} {
			if err := os.MkdirAll(filepath.Join(directory, suffix), 0o700); err != nil {
				return err
			}
		}
		allRoots = append(allRoots, directory)
		selection, _ := team.Role(name)
		cwd := directory
		if name == "implementation" {
			cwd = state.Worktree
		}
		member, err := (mail.FileRoster{Dir: ledger}).Register(mail.Worker{Name: name, Harness: mail.Harness(selection.Harness), Worktree: cwd, Coordinator: name == "coordinator", Socket: filepath.Join(engine.socketRoot, name+".sock")}, false)
		if err != nil {
			return err
		}
		engine.roles[name] = &nativeRole{Name: name, Directory: directory, Member: member}
	}
	host, err := goalnative.StartCommandHost(ctx, goalnative.Config{Binary: engine.codex, SupervisorBinary: engine.self, ControlDir: filepath.Join(state.RunDir, "native-control", "execution"), Cwd: state.Worktree, Socket: filepath.Join(engine.socketRoot, "exec.sock"), WritableRoots: allRoots})
	if err != nil {
		return fmt.Errorf("non-model execution boundary: %w", err)
	}
	engine.commandHost = host
	adapter := &nativeMailboxAdapter{engine: engine, context: ctx}
	engine.service = &mail.Service{Box: mail.Mailbox{Dir: ledger, Private: true}, Roster: mail.FileRoster{Dir: ledger}, Adapters: map[mail.Harness]mail.Adapter{mail.HarnessCodex: adapter, mail.HarnessPi: adapter, mail.HarnessClaude: adapter}, Policy: mail.DefaultPolicy(), ReplyBinary: engine.self}
	for _, name := range []string{"coordinator", "implementation", "reviewer"} {
		role := engine.roles[name]
		role.Executor = host
		roots := []string{role.Directory}
		if name == "implementation" {
			roots = append(roots, state.Worktree)
		}
		if err := engine.probeBoundary(ctx, role, roots); err != nil {
			return err
		}
		control := filepath.Join(state.RunDir, "native-control", name)
		if err := os.MkdirAll(control, 0o700); err != nil {
			return err
		}
		toolSocket, capabilityFile := filepath.Join(engine.socketRoot, name+"-tools.sock"), filepath.Join(control, "tool-capability")
		server, err := goaltools.Start(ctx, toolSocket, capabilityFile, engine.toolHandler(role, roots))
		if err != nil {
			return err
		}
		role.Tools = server
		selection, _ := team.Role(name)
		environment := append(cleanEnvironment(os.Environ()), "TMPDIR="+filepath.Join(role.Directory, "tmp"), "GOCACHE="+filepath.Join(role.Directory, "cache"))
		observer := func(event goalnative.NativeEvent) { engine.normalizedEvent(name, event) }
		rawObserver := func(event json.RawMessage) {
			if err := engine.record(Event{Time: time.Now().UTC(), Kind: "native", Role: name, Data: event}); err != nil {
				engine.fail(err)
			}
		}
		var session goalnative.NativeSession
		if selection.Harness == "codex" {
			session, err = goalnative.StartCodex(ctx, goalnative.Config{Binary: engine.codex, SupervisorBinary: engine.self, ControlDir: control, Cwd: role.Member.Worktree, Socket: role.Member.Socket, Model: selection.Model, Environment: environment, WritableRoots: roots, ToolSocket: toolSocket, ToolCapabilityFile: capabilityFile, OnEvent: rawObserver}, observer)
		} else {
			binary, found := engine.nativeBinaries[selection.Harness]
			if !found {
				return fmt.Errorf("%s has no verified native executable", selection.Harness)
			}
			config := goalnative.NativeConfig{Binary: binary.Path, SupervisorBinary: engine.self, ControlDir: control, Cwd: role.Member.Worktree, Socket: role.Member.Socket, Provider: selection.Provider, Model: selection.Model, Environment: environment, ToolSocket: toolSocket, ToolCapabilityFile: capabilityFile, OnEvent: observer, OnRawEvent: rawObserver}
			switch selection.Harness {
			case "pi":
				session, err = goalnative.StartPi(ctx, config)
			case "claude":
				session, err = goalnative.StartClaude(ctx, config)
			default:
				return errors.New("unsupported native goal harness")
			}
		}
		if err != nil {
			return fmt.Errorf("%s %s startup: %w", name, selection.Harness, err)
		}
		role.Session = &trackedNativeSession{role: name, engine: engine, native: session}
		identity := session.Identity()
		if identity.Harness != selection.Harness || identity.Model != selection.Model || identity.Provider != selection.Provider || identity.SessionID == "" {
			return errors.New("native team member did not retain its approved identity")
		}
		role.Member.ThreadID = identity.SessionID
		member, err := (mail.FileRoster{Dir: ledger}).Register(role.Member, true)
		if err != nil {
			return err
		}
		role.Member = member
		if _, _, err := (project.Store{Root: engine.store.Root}).Bind(ctx, created.ID, threads[name], name, member.RunID, ""); err != nil {
			return err
		}
		if err := engine.update(func(state *State) {
			state.Roles = append(state.Roles, Role{Name: name, ThreadID: threads[name], RunID: member.RunID, NativeSessionID: identity.SessionID, Socket: member.Socket, Provider: identity.Provider, Model: identity.Model, Harness: identity.Harness})
		}); err != nil {
			return err
		}
	}
	return nil
}

func (engine *execution) normalizedEvent(role string, event goalnative.NativeEvent) {
	// termsafe:allow-raw-json private normalized event evidence, never terminal output
	data, err := json.Marshal(event)
	if err != nil {
		engine.fail(err)
		return
	}
	if err := engine.record(Event{Time: time.Now().UTC(), Kind: "native-normalized", Role: role, Data: data}); err != nil {
		engine.fail(err)
		return
	}
	if event.RequestID == "" {
		return
	}
	engine.mutex.Lock()
	defer engine.mutex.Unlock()
	matched := false
	for _, member := range engine.state.Roles {
		if member.Name == role && member.NativeSessionID == event.Identity.SessionID && member.Provider == event.Identity.Provider && member.Model == event.Identity.Model && member.Harness == event.Identity.Harness {
			matched = true
		}
	}
	if !matched {
		engine.fail(errors.New("native event did not match the bound role identity"))
		return
	}
	trace := engine.traces[role]
	if trace == nil {
		trace = &nativeTrace{Turns: map[string]nativeTurn{}, Digests: map[string]string{}, Expected: map[string]string{}, Consumed: map[string]bool{}}
		engine.traces[role] = trace
	}
	if trace.Expected == nil {
		trace.Expected = map[string]string{}
	}
	if trace.Consumed == nil {
		trace.Consumed = map[string]bool{}
	}
	digest, expected := trace.Expected[event.RequestID]
	if !expected {
		engine.fail(errors.New("native event did not match a registered request"))
		return
	}
	if event.InputSHA256 != "" && event.InputSHA256 != digest {
		engine.fail(errors.New("native event input digest did not match its registered request"))
		return
	}
	turn := trace.Turns[event.RequestID]
	trace.Digests[event.RequestID] = digest
	switch event.Status {
	case "accepted":
		if turn.Status == "completed" || turn.Status == "failed" || turn.Status == "aborted" || turn.Status == "error" {
			engine.fail(errors.New("native event reopened a terminal request"))
			return
		}
		if turn.Status == "inProgress" {
			return
		}
		turn.Status = "inProgress"
	case "consumed":
		if trace.Consumed[event.RequestID] {
			return
		}
		if turn.Status == "completed" || turn.Status == "failed" || turn.Status == "aborted" || turn.Status == "error" {
			engine.fail(errors.New("native event changed a terminal request"))
			return
		}
		trace.Consumed[event.RequestID] = true
		trace.Inputs = append(trace.Inputs, nativeInput{TurnID: event.RequestID, Content: trace.Digests[event.RequestID]})
	case "completed":
		if turn.Status == "completed" {
			if turn.Text == event.Text && turn.ID == event.NativeID {
				return
			}
			engine.fail(errors.New("conflicting duplicate native completion"))
			return
		}
		if turn.Status == "failed" || turn.Status == "aborted" || turn.Status == "error" {
			engine.fail(errors.New("native event changed a terminal request"))
			return
		}
		if !event.Settled || event.NativeID == "" || event.Text == "" {
			engine.fail(errors.New("native completion lacks settled identity or final text"))
			return
		}
		turn.ID, turn.Status, turn.Text = event.NativeID, "completed", event.Text
	case "failed", "aborted", "error":
		if turn.Status == "completed" || turn.Status == "failed" || turn.Status == "aborted" || turn.Status == "error" {
			if turn.Status == event.Status && turn.Error == event.Text {
				return
			}
			engine.fail(errors.New("conflicting duplicate native terminal event"))
			return
		}
		turn.Status, turn.Error = event.Status, event.Text
	default:
		return
	}
	trace.Turns[event.RequestID] = turn
	trace.Latest = event.RequestID
}

func (engine *execution) registerNativePrompt(role string, prompt goalnative.NativePrompt) error {
	if err := goalnative.ValidatePrompt(prompt); err != nil {
		return err
	}
	engine.mutex.Lock()
	defer engine.mutex.Unlock()
	trace := engine.traces[role]
	if trace == nil {
		trace = &nativeTrace{Turns: map[string]nativeTurn{}, Digests: map[string]string{}, Expected: map[string]string{}, Consumed: map[string]bool{}}
		engine.traces[role] = trace
	}
	if trace.Expected == nil {
		trace.Expected = map[string]string{}
	}
	if trace.Consumed == nil {
		trace.Consumed = map[string]bool{}
	}
	if _, exists := trace.Expected[prompt.ID]; exists {
		return errors.New("native request identity was already registered")
	}
	trace.Expected[prompt.ID] = goalnative.PromptDigest(prompt.Text)
	trace.Digests[prompt.ID] = goalnative.PromptDigest(prompt.Text)
	return nil
}

type trackedNativeSession struct {
	role   string
	engine *execution
	native goalnative.NativeSession
}

func (session *trackedNativeSession) Identity() goalnative.NativeIdentity {
	return session.native.Identity()
}

func (session *trackedNativeSession) Submit(ctx context.Context, prompt goalnative.NativePrompt) error {
	if err := session.engine.registerNativePrompt(session.role, prompt); err != nil {
		return err
	}
	return session.native.Submit(ctx, prompt)
}

func (session *trackedNativeSession) Wait(ctx context.Context, requestID string) (goalnative.TurnResult, error) {
	return session.native.Wait(ctx, requestID)
}

func (session *trackedNativeSession) Turn(ctx context.Context, text string) (goalnative.TurnResult, error) {
	prompt, err := goalnative.NewPrompt(text)
	if err != nil {
		return goalnative.TurnResult{}, err
	}
	if err := session.Submit(ctx, prompt); err != nil {
		return goalnative.TurnResult{}, err
	}
	return session.Wait(ctx, prompt.ID)
}

func (session *trackedNativeSession) WaitIdle(ctx context.Context) error {
	return session.native.WaitIdle(ctx)
}
func (session *trackedNativeSession) Close(ctx context.Context) error {
	return session.native.Close(ctx)
}

type nativeMailboxAdapter struct {
	engine  *execution
	context context.Context
}

func (adapter *nativeMailboxAdapter) member(worker mail.Worker) (goalnative.NativeSession, error) {
	role := adapter.engine.roles[worker.Name]
	if role == nil || role.isClosed() || role.Member.RunID != worker.RunID || role.Member.ThreadID != worker.ThreadID || role.Member.Harness != worker.Harness {
		return nil, errors.New("native mailbox incarnation does not match its owned session")
	}
	session, valid := role.Session.(goalnative.NativeSession)
	if !valid {
		return nil, errors.New("native mailbox session is not available")
	}
	return session, nil
}

func (adapter *nativeMailboxAdapter) State(ctx context.Context, worker mail.Worker) (mail.WorkerState, error) {
	session, err := adapter.member(worker)
	if err != nil {
		return mail.StateAbsent, err
	}
	probe, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	if err := session.WaitIdle(probe); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return mail.StateBusy, nil
		}
		return mail.StateUnknown, err
	}
	return mail.StateIdle, nil
}

func (adapter *nativeMailboxAdapter) Deliver(ctx context.Context, worker mail.Worker, _ mail.Message, text string) (string, error) {
	session, err := adapter.member(worker)
	if err != nil {
		return "", err
	}
	prompt, err := goalnative.NewPrompt(text)
	if err != nil {
		return "", err
	}
	if err := session.Submit(ctx, prompt); err != nil {
		return "", mail.Unconfirmed("native prompt acceptance is unresolved: %v", err)
	}
	go func() {
		if _, err := session.Wait(adapter.context, prompt.ID); err != nil {
			adapter.engine.fail(fmt.Errorf("%s native handling: %w", worker.Name, err))
		}
	}()
	return "native accepted request " + prompt.ID + "; handling not yet complete", nil
}
