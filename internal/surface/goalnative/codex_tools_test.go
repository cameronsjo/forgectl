//go:build unix

package goalnative

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/goaltools"
)

func TestCodexBrokerRequiresObservedDisabledFeatures(t *testing.T) {
	features := make(map[string]bool)
	for _, feature := range brokerDisabledFeatures {
		features[feature] = false
	}
	features["code_mode_host"] = true
	configuration := map[string]any{"config": map[string]any{"features": features, "tools": map[string]bool{"view_image": false}}}
	if err := assertBrokerTools(mustJSON(configuration)); err != nil {
		t.Fatal(err)
	}
	features["code_mode_host"] = false
	if err := assertBrokerTools(mustJSON(configuration)); err == nil {
		t.Fatal("disabled code-mode dispatcher was accepted")
	}
	delete(features, "code_mode_host")
	if err := assertBrokerTools(mustJSON(configuration)); err == nil {
		t.Fatal("missing code-mode dispatcher observation was accepted")
	}
	features["code_mode_host"] = true
	for _, feature := range brokerDisabledFeatures {
		features[feature] = true
		if err := assertBrokerTools(mustJSON(configuration)); err == nil {
			t.Fatalf("enabled %s was accepted", feature)
		}
		delete(features, feature)
		if err := assertBrokerTools(mustJSON(configuration)); err == nil {
			t.Fatalf("missing %s observation was accepted", feature)
		}
		features[feature] = false
	}
	definitions := dynamicGoalTools()
	if len(definitions) != len(GoalToolNames) {
		t.Fatalf("dynamic goal tools = %#v", definitions)
	}
	for index, name := range GoalToolNames {
		if definitions[index]["name"] != name || definitions[index]["type"] != "function" || definitions[index]["deferLoading"] != false {
			t.Fatalf("dynamic goal tool %s = %#v", name, definitions[index])
		}
	}
}

func TestCodexRealBrokerToolInventory(t *testing.T) {
	if os.Getenv("FORGECTL_GOALNATIVE_INTEGRATION") != "1" {
		t.Skip("set FORGECTL_GOALNATIVE_INTEGRATION=1 for the local no-model tool inventory")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	observed := make(chan []string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Tools []struct{ Name, Type string } `json:"tools"`
		}
		if request.URL.Path != "/responses" || json.NewDecoder(io.LimitReader(request.Body, 2<<20)).Decode(&body) != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		names := make([]string, 0, len(body.Tools))
		for _, tool := range body.Tools {
			names = append(names, tool.Name)
		}
		select {
		case observed <- names:
		default:
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, `{"error":{"message":"local no-model tool inventory complete","type":"invalid_request_error"}}`)
	}))
	defer server.Close()
	environment := CredentialFreeEnvironment(os.Environ(), root)
	arguments, err := nativeServerArguments(hostSpec{Cwd: root, Socket: filepath.Join(root, "unused.sock"), BrokerTools: true}, environment)
	if err != nil {
		t.Fatal(err)
	}
	arguments[len(arguments)-1] = "stdio://"
	arguments = append(arguments, "--config", `model_provider="probe"`, "--config", fmt.Sprintf(`model_providers.probe={name="local no-model probe",base_url=%q,wire_api="responses",requires_openai_auth=false,request_max_retries=0}`, server.URL))
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, arguments...) //nolint:gosec // Explicit native binary and fixed test arguments.
	command.Env, command.Dir = environment, root
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = input.Close()
		cancel()
		_ = command.Wait()
	}()
	encoder, scanner := json.NewEncoder(input), bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 8192), maxPayloadBytes)
	nextID := 0
	call := func(method string, params any) json.RawMessage {
		t.Helper()
		nextID++
		if err := encoder.Encode(map[string]any{"id": nextID, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		for scanner.Scan() {
			var response rpcEnvelope
			if json.Unmarshal(scanner.Bytes(), &response) != nil {
				t.Fatal("invalid native probe response")
			}
			if string(response.ID) != fmt.Sprint(nextID) {
				continue
			}
			if len(response.Error) > 0 {
				t.Fatalf("native inventory %s: %s", method, response.Error)
			}
			return response.Result
		}
		t.Fatalf("native inventory %s ended before reply: %v", method, scanner.Err())
		return nil
	}
	call("initialize", map[string]any{"clientInfo": map[string]string{"name": "forgectl-goal-probe", "version": "dev"}, "capabilities": map[string]bool{"experimentalApi": true}})
	thread := call("thread/start", map[string]any{"cwd": root, "model": "gpt-6.1-sol", "modelProvider": "probe", "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": true, "allowProviderModelFallback": false, "dynamicTools": dynamicGoalTools()})
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(thread, &started) != nil || started.Thread.ID == "" {
		t.Fatal("native inventory thread has no identity")
	}
	call("turn/start", map[string]any{"threadId": started.Thread.ID, "input": []map[string]string{{"type": "text", "text": "local no-model tool inventory probe"}}})
	select {
	case tools := <-observed:
		wanted := append(append([]string(nil), GoalToolNames...), "request_user_input")
		slices.Sort(tools)
		slices.Sort(wanted)
		if !slices.Equal(tools, wanted) {
			t.Fatalf("actual native model tool inventory = %v, want only %v (user requests are declined)", tools, wanted)
		}
	case <-ctx.Done():
		t.Fatal("native inventory did not reach the credential-free local mock provider")
	}
}

func TestCodexGoalToolRejectsReplayAndForeignTurn(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "gn-tools-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket, capability := filepath.Join(root, "tool.sock"), filepath.Join(root, "capability")
	var calls atomic.Int32
	server, err := goaltools.Start(t.Context(), socket, capability, func(_ context.Context, operation string, arguments json.RawMessage) (any, error) {
		calls.Add(1)
		return map[string]any{"operation": operation, "arguments": arguments}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	session := &Session{ThreadID: "owned-thread", config: Config{ToolSocket: socket, ToolCapabilityFile: capability}, nativeBusy: map[string]struct{}{"owned-turn": {}}, toolCalls: map[string]struct{}{}}
	request := map[string]any{"threadId": "owned-thread", "turnId": "owned-turn", "callId": "one", "tool": "goal_inbox", "arguments": map[string]any{}}
	if _, err := session.callGoalTool(t.Context(), mustJSON(request)); err != nil || calls.Load() != 1 {
		t.Fatalf("owned live call: %v, calls=%d", err, calls.Load())
	}
	if _, err := session.callGoalTool(t.Context(), mustJSON(request)); err == nil || calls.Load() != 1 {
		t.Fatal("replayed native call reached the broker")
	}
	request["callId"] = "two"
	for key, value := range map[string]string{"threadId": "foreign-thread", "turnId": "foreign-turn", "tool": "exec_command", "namespace": "other", "extra": "unknown"} {
		original, existed := request[key]
		request[key] = value
		if _, err := session.callGoalTool(t.Context(), mustJSON(request)); err == nil || calls.Load() != 1 {
			t.Fatalf("invalid %s reached the broker", key)
		}
		if existed {
			request[key] = original
		} else {
			delete(request, key)
		}
	}
	session.closed.Store(true)
	if _, err := session.callGoalTool(t.Context(), mustJSON(request)); err == nil || calls.Load() != 1 {
		t.Fatal("closed native session reached the broker")
	}
}

func TestCodexRealBrokerStartupNoPrompt(t *testing.T) {
	if os.Getenv("FORGECTL_GOALNATIVE_INTEGRATION") != "1" {
		t.Skip("set FORGECTL_GOALNATIVE_INTEGRATION=1 for the real Codex startup probe")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/private/tmp", "gn-codex-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	session, err := StartCodex(t.Context(), Config{Binary: binary, SupervisorBinary: writeHelper(t, "supervisor", "GOALNATIVE_HOST_HELPER=1"), ControlDir: filepath.Join(root, "control"), Cwd: work, Socket: filepath.Join(root, "codex.sock"), Model: "gpt-6.1-sol", WritableRoots: []string{work}, ToolSocket: filepath.Join(root, "tools.sock"), ToolCapabilityFile: filepath.Join(root, "capability")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := session.Close(cleanup); err != nil {
		t.Fatal(err)
	}
}
