package goalnative

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/goaltools"
)

func TestValidatePiConfigRequiresExplicitOpenAIRoute(t *testing.T) {
	root := t.TempDir()
	valid := NativeConfig{
		Binary:             filepath.Join(root, "pi"),
		SupervisorBinary:   filepath.Join(root, "forgectl"),
		ControlDir:         filepath.Join(root, "control"),
		Cwd:                root,
		Socket:             filepath.Join(root, "app.sock"),
		Provider:           "openai",
		Model:              "gpt-5.6-sol",
		ToolSocket:         filepath.Join(root, "tools.sock"),
		ToolCapabilityFile: filepath.Join(root, "capability.json"),
	}
	if err := validatePiConfig(valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, provider := range []string{"anthropic", "google", "", "openai-compatible"} {
		invalid := valid
		invalid.Provider = provider
		if err := validatePiConfig(invalid); err == nil {
			t.Errorf("provider %q was accepted", provider)
		}
	}
	for _, field := range []struct {
		name  string
		value string
		set   func(*NativeConfig, string)
	}{
		{"relative cwd", "relative", func(config *NativeConfig, value string) { config.Cwd = value }},
		{"unclean socket", root + "/../" + filepath.Base(root) + "/app.sock", func(config *NativeConfig, value string) { config.Socket = value }},
		{"relative capability file", "capability.json", func(config *NativeConfig, value string) { config.ToolCapabilityFile = value }},
	} {
		invalid := valid
		field.set(&invalid, field.value)
		if err := validatePiConfig(invalid); err == nil {
			t.Errorf("%s was accepted", field.name)
		}
	}
}

func TestPiEmbeddedToolsUseActualBrokerContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required to execute the embedded Pi extension")
	}
	root := piIntegrationDir(t)
	control := filepath.Join(root, "control")
	if err := preparePiFiles(NativeConfig{ControlDir: control}); err != nil {
		t.Fatal(err)
	}
	socket, capability := filepath.Join(root, "tools.sock"), filepath.Join(root, "capability")
	server, err := goaltools.Start(t.Context(), socket, capability, func(_ context.Context, name string, arguments json.RawMessage) (any, error) {
		return map[string]any{"name": name, "arguments": arguments}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close(context.Background()) }()
	code := `import {pathToFileURL} from "node:url";
const tools=[]; const handlers=new Map();
const register=(await import(pathToFileURL(process.argv[1]))).default;
register({registerTool: tool=>tools.push(tool), on: (name,handler)=>handlers.set(name,handler)});
const args={goal_exec:{argv:["/bin/echo","ok"],timeout_seconds:1},goal_read:{path:"README.md"},goal_write:{path:"README.md",content:"ok"},goal_mail:{to:"coordinator",body:"STATUS ok"},goal_inbox:{}};
const results=[];
for(const tool of tools){const result=await tool.execute("native-call",args[tool.name]);results.push(JSON.parse(result.content[0].text));}
process.stdout.write(JSON.stringify({definitions:tools.map(tool=>({name:tool.name,description:tool.description,inputSchema:tool.parameters})),results}));`
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "--input-type=module", "-e", code, filepath.Join(control, "pi-goal-tools.mjs")) //nolint:gosec // Runs the embedded extension against a test-owned broker.
	command.Env = append(CredentialFreeEnvironment(os.Environ(), root), "FORGECTL_GOAL_TOOL_SOCKET="+socket, "FORGECTL_GOAL_TOOL_CAPABILITY_FILE="+capability)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("execute actual embedded tools against actual broker: %s, %v", output, err)
	}
	var observed struct {
		Definitions []map[string]any `json:"definitions"`
		Results     []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"results"`
	}
	if err := json.Unmarshal(output, &observed); err != nil {
		t.Fatal(err)
	}
	var expected []map[string]any
	if err := json.Unmarshal(mustJSON(goaltools.Definitions()), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(observed.Definitions, expected) || len(observed.Results) != len(GoalToolNames) {
		t.Fatalf("embedded tool contract drifted: %s", output)
	}
	for index, name := range GoalToolNames {
		if observed.Results[index].Name != name || len(observed.Results[index].Arguments) == 0 {
			t.Fatalf("embedded tool %s lost its result: %s", name, output)
		}
	}
}

func TestValidatePiModelRouteRejectsProxyAndCredentialVariants(t *testing.T) {
	if err := validatePiModelRoute("openai", "https://api.openai.com"); err != nil {
		t.Fatal(err)
	}
	for _, baseURL := range []string{
		"http://api.openai.com",
		"https://api.openai.com?proxy=1",
		"https://user:secret@api.openai.com",
		"https://evil.example/api.openai.com",
		"https://api.openai.com:443",
	} {
		if err := validatePiModelRoute("openai", baseURL); err == nil {
			t.Errorf("route %q was accepted", baseURL)
		}
	}
	if err := validatePiModelRoute("openai-codex", "https://chatgpt.com"); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePiSessionFileRequiresPrivateFileInsidePrivateDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // Owner-private test directory requires traversal.
		t.Fatal(err)
	}
	path := filepath.Join(root, "session.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePiSessionFile(path, root); err != nil {
		t.Fatal(err)
	}
	lazy := filepath.Join(root, "lazy-session.jsonl")
	if err := validatePiSessionFile(lazy, root); err != nil {
		t.Fatalf("lazy session path rejected: %v", err)
	}
	outsideRoot := t.TempDir()
	outside := filepath.Join(outsideRoot, "session.json")
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, outside := range []string{outside, root + "/./session.json"} {
		if err := validatePiSessionFile(outside, root); err == nil {
			t.Errorf("outside path %q was accepted", outside)
		}
	}
	public := filepath.Join(root, "public.json")
	if err := os.WriteFile(public, []byte("{}"), 0o644); err != nil { //nolint:gosec // Deliberately public negative-control fixture.
		t.Fatal(err)
	}
	if err := validatePiSessionFile(public, root); err == nil {
		t.Fatal("world-readable session file was accepted")
	}
}

func TestPiFulfilledOutcomeIsRetainedAfterWait(t *testing.T) {
	promptID := strings.Repeat("b", 32)
	session := &piSession{pending: map[string]*piPending{}, outcomes: map[string]piCompletion{}, wake: make(chan struct{}), done: make(chan struct{})}
	session.pending[promptID] = &piPending{completed: make(chan piCompletion, 1)}
	session.complete(promptID, TurnResult{ID: "native-entry", Status: "completed", Text: "done"}, nil)
	first, err := session.Wait(context.Background(), promptID)
	if err != nil || first.ID != "native-entry" {
		t.Fatalf("first wait = %#v, %v", first, err)
	}
	second, err := session.Wait(context.Background(), promptID)
	if err != nil || second.ID != "native-entry" {
		t.Fatalf("fulfilled wait = %#v, %v", second, err)
	}
}

func TestPiCompletionIsObservedBeforeSuccessfulWait(t *testing.T) {
	promptID := strings.Repeat("c", 32)
	entered, release := make(chan struct{}), make(chan struct{})
	session := &piSession{pending: map[string]*piPending{}, wake: make(chan struct{}), done: make(chan struct{})}
	session.pending[promptID] = &piPending{completed: make(chan piCompletion, 1), inputHash: PromptDigest("probe")}
	session.config.OnEvent = func(event NativeEvent) {
		_ = session.Identity()
		if event.Status != "completed" || event.RequestID != promptID {
			t.Errorf("unexpected completion event: %#v", event)
		}
		close(entered)
		<-release
	}
	published := make(chan struct{})
	go func() {
		session.publishCompletion(promptID, "leaf", TurnResult{ID: "native-entry", Status: "completed", Text: "done"}, nil)
		close(published)
	}()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := session.Wait(ctx, promptID); err == nil {
		t.Errorf("Wait succeeded before the observer returned: %#v", result)
	}
	session.mu.Lock()
	_, earlyOutcome := session.outcomes[promptID]
	session.mu.Unlock()
	if earlyOutcome {
		t.Error("completion was exposed before publication")
	}
	close(release)
	<-published
	result, err := session.Wait(t.Context(), promptID)
	if err != nil || result.ID != "native-entry" {
		t.Fatalf("published completion = %#v, %v", result, err)
	}
	result, err = session.Wait(t.Context(), promptID)
	if err != nil || result.ID != "native-entry" {
		t.Fatalf("retained published completion = %#v, %v", result, err)
	}
}

func TestPiEntriesRequireExactToolAttestation(t *testing.T) {
	data, err := json.Marshal(piToolAttestation{Version: 1, Tools: sortedPiTools()})
	if err != nil {
		t.Fatal(err)
	}
	entries := piEntries{LeafID: "leaf", Entries: []piEntry{{Type: "custom", ID: "attestation", CustomType: piToolAttestationType, Data: data}}}
	if err := validatePiEntries(entries, "", true); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(piToolAttestation{Version: 1, Tools: []string{"goal_exec"}})
	if err != nil {
		t.Fatal(err)
	}
	entries.Entries[0].Data = data
	if err := validatePiEntries(entries, "", true); err == nil {
		t.Fatal("incomplete tool attestation was accepted")
	}
}

func TestPiTurnSettlesOnlyAfterConsumedFinalAssistantAndAgentSettled(t *testing.T) {
	prompt := NativePrompt{ID: strings.Repeat("a", 32), Text: "inspect the repository"}
	var events []NativeEvent
	session := &piSession{config: NativeConfig{OnEvent: func(event NativeEvent) { events = append(events, event) }}, pending: map[string]*piPending{}, requests: map[string]chan piRPCResult{}, wake: make(chan struct{}), done: make(chan struct{})}
	pending := &piPending{prompt: prompt, accepted: make(chan error, 1), completed: make(chan piCompletion, 1), inputHash: PromptDigest(prompt.Text)}
	session.pending[prompt.ID] = pending

	session.handleEvent(json.RawMessage(`{"type":"response","id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","command":"prompt","success":true}`))
	if err := <-pending.accepted; err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Status != "accepted" || events[0].RequestID != prompt.ID {
		t.Fatalf("acceptance events = %#v", events)
	}
	session.handleEvent(json.RawMessage(`{"type":"agent_start"}`))
	session.handleEvent(json.RawMessage(`{"type":"message_start","message":{"role":"user","content":"inspect the repository"}}`))
	if len(events) != 2 || events[1].Status != "consumed" {
		t.Fatalf("consumption events = %#v", events)
	}
	session.handleEvent(json.RawMessage(`{"type":"turn_end","turnIndex":1,"message":{"role":"assistant","id":"native-turn","stopReason":"stop","content":"verified"}}`))
	select {
	case result := <-pending.completed:
		t.Fatalf("turn completed before agent settled: %#v", result)
	default:
	}
	session.handleEvent(json.RawMessage(`{"type":"agent_settled"}`))
	if !pending.finalizing {
		t.Fatal("settled prompt was not marked for native entry verification")
	}
	if len(events) != 2 {
		t.Fatalf("completion events = %#v", events)
	}
	turn, err := finalPiTurn(piEntries{Entries: []piEntry{
		{Type: "message", ID: "user-entry", Message: &piMessage{Role: "user", Content: prompt.Text}},
		{Type: "message", ID: "assistant-entry", Message: &piMessage{Role: "assistant", StopReason: "stop", Content: "verified"}},
	}}, PromptDigest(prompt.Text))
	if err != nil || turn.ID != "assistant-entry" || turn.Status != "completed" || turn.Text != "verified" {
		t.Fatalf("native entry turn = %#v, %v", turn, err)
	}
}

func TestPiRejectsUnapprovedToolAndUnexpectedModelChange(t *testing.T) {
	session := &piSession{pending: map[string]*piPending{}, requests: map[string]chan piRPCResult{}, wake: make(chan struct{}), done: make(chan struct{})}
	session.handleEvent(json.RawMessage(`{"type":"tool_execution_start","toolName":"shell"}`))
	if !session.closed || session.closeErr == nil || !strings.Contains(session.closeErr.Error(), "unapproved tool") {
		t.Fatalf("unapproved tool state = closed:%v err:%v", session.closed, session.closeErr)
	}
	session = &piSession{pending: map[string]*piPending{}, requests: map[string]chan piRPCResult{}, wake: make(chan struct{}), done: make(chan struct{})}
	session.handleEvent(json.RawMessage(`{"type":"model_select"}`))
	if !session.closed || session.closeErr == nil || !strings.Contains(session.closeErr.Error(), "changed its model") {
		t.Fatalf("model change state = closed:%v err:%v", session.closed, session.closeErr)
	}
}

func TestPiGoalExtensionIsEmbeddedAndCapabilityBound(t *testing.T) {
	source := string(piGoalToolsSource)
	for _, marker := range []string{
		"export default function",
		"FORGECTL_GOAL_TOOL_SOCKET",
		"FORGECTL_GOAL_TOOL_CAPABILITY_FILE",
		"goal_exec",
		"goal_read",
		"goal_write",
		"goal_mail",
		"goal_inbox",
		"request_id",
		"pi-goal-tools.json",
	} {
		if !strings.Contains(source, marker) {
			t.Errorf("embedded extension is missing %q", marker)
		}
	}
	for _, forbidden := range []string{"child_process", "fetch(", "eval(", "Function("} {
		if strings.Contains(source, forbidden) {
			t.Errorf("embedded extension uses forbidden capability %q", forbidden)
		}
	}
}

func TestSanitizePiEventRemovesCredentialFields(t *testing.T) {
	safe := string(sanitizePiEvent(json.RawMessage(`{"type":"tool_execution_end","token":"secret","nested":{"authorization":"bearer secret","value":"keep"}}`)))
	if strings.Contains(safe, "secret") || strings.Contains(safe, "authorization") || strings.Contains(safe, "token") {
		t.Fatalf("credential field survived sanitization: %s", safe)
	}
	if !strings.Contains(safe, "keep") {
		t.Fatalf("ordinary event field was removed: %s", safe)
	}
}

func TestPiRealStartupUsesPrivateSessionAndBroker(t *testing.T) {
	if os.Getenv("FORGECTL_PI_INTEGRATION") != "1" {
		t.Skip("set FORGECTL_PI_INTEGRATION=1 for the real Pi startup boundary")
	}
	root := piIntegrationDir(t)
	control := filepath.Join(root, "control")
	if err := os.Mkdir(control, 0o700); err != nil {
		t.Fatal(err)
	}
	toolSocket := filepath.Join(root, "tools.sock")
	capabilityFile := filepath.Join(root, "capability")
	broker, err := goaltools.Start(context.Background(), toolSocket, capabilityFile, func(context.Context, string, json.RawMessage) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeContext, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := broker.Close(closeContext); err != nil {
			t.Error(err)
		}
	}()
	piBinary := os.Getenv("FORGECTL_PI_BINARY")
	if piBinary == "" {
		piBinary = "/opt/homebrew/bin/pi"
	}
	supervisor := os.Getenv("FORGECTL_PI_SUPERVISOR")
	if supervisor == "" {
		supervisor = "/private/tmp/forgectl-native-teams-dev"
	}
	socket := filepath.Join(root, "pi.sock")
	config := NativeConfig{Binary: piBinary, SupervisorBinary: supervisor, ControlDir: control, Cwd: mustCurrentDirectory(t), Socket: socket, Provider: "openai-codex", Model: "gpt-6.1-sol", Environment: os.Environ(), ToolSocket: toolSocket, ToolCapabilityFile: capabilityFile}
	session, err := StartPi(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := session.Close(closeContext); err != nil {
			t.Error(err)
		}
	}()
	identity := session.Identity()
	if identity.Harness != "pi" || identity.Provider != config.Provider || identity.Model != config.Model || identity.SessionID == "" {
		t.Fatalf("identity = %#v", identity)
	}
}

func piIntegrationDir(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/private/tmp", "pi-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // Owner-private test directory requires traversal.
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func mustCurrentDirectory(t *testing.T) string {
	t.Helper()
	path, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return path
}
