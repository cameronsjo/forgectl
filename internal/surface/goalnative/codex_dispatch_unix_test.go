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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/goaltools"
)

func TestCodexRealCodeModeCallsOnlyBrokerTools(t *testing.T) {
	if os.Getenv("FORGECTL_GOALNATIVE_INTEGRATION") != "1" {
		t.Skip("set FORGECTL_GOALNATIVE_INTEGRATION=1 for real no-model code-mode dispatch")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "gn-dispatch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	var brokerCalls atomic.Int32
	socket, capabilityFile := filepath.Join(root, "tools.sock"), filepath.Join(root, "capability")
	broker, err := goaltools.Start(t.Context(), socket, capabilityFile, func(_ context.Context, operation string, arguments json.RawMessage) (any, error) {
		if operation != "goal_inbox" || string(arguments) != "{}" {
			return nil, fmt.Errorf("unexpected broker call %s: %s", operation, arguments)
		}
		brokerCalls.Add(1)
		return map[string]string{"receipt": "BROKER-REACHED"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close(context.Background()) })
	capability, err := goaltools.ReadCapability(capabilityFile)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan json.RawMessage, 4)
	var requestCount atomic.Int32
	var imports atomic.Int32
	modulePath := filepath.Join(root, "module.mjs")
	if err := os.WriteFile(modulePath, []byte(`export default "IMPORT-WAS-ALLOWED";`), 0o600); err != nil {
		t.Fatal(err)
	}
	code := `if(typeof process!=="undefined"||typeof require!=="undefined"||typeof fetch!=="undefined")throw Error("ambient I/O");
const expected=["goal_exec","goal_inbox","goal_mail","goal_read","goal_write"];
if(JSON.stringify(ALL_TOOLS.map(tool=>tool.name).sort())!==JSON.stringify(expected))throw Error("unexpected nested tools");
for(const name of ["exec_command","apply_patch","web_search","spawn_agent"])if(typeof tools[name]!=="undefined")throw Error("native tool escape");
for(const module of ["node:fs","node:child_process",%q,%q]){let denied=false;try{await import(module)}catch{denied=true}if(!denied)throw Error("module import escape")}
text("ISOLATE-BOUNDS-VERIFIED");text(await tools.goal_inbox({}));`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/probe" {
			imports.Add(1)
			writer.Header().Set("Content-Type", "text/javascript")
			_, _ = io.WriteString(writer, `export default "IMPORT-WAS-ALLOWED";`)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, 2<<20))
		if err != nil || request.URL.Path != "/responses" || !json.Valid(body) {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- body
		sequence := requestCount.Add(1)
		item := map[string]any{"type": "custom_tool_call", "id": "item-tool", "call_id": "call-probe", "name": "exec", "namespace": "functions", "input": code}
		if sequence > 1 {
			item = map[string]any{"type": "message", "id": "item-final", "role": "assistant", "status": "completed", "content": []map[string]string{{"type": "output_text", "text": "NATIVE-DISPATCH-COMPLETE"}}}
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": fmt.Sprintf("response-%d", sequence), "status": "in_progress"}},
			{"type": "response.output_item.added", "output_index": 0, "item": item},
			{"type": "response.output_item.done", "output_index": 0, "item": item},
			{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("response-%d", sequence), "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
		} {
			_, _ = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", frame["type"], mustJSON(frame))
		}
	}))
	defer server.Close()
	code = fmt.Sprintf(code, server.URL+"/probe", "file://"+modulePath)
	positive, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(positive)
	if err != nil {
		t.Fatal(err)
	}
	positiveBody, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || string(positiveBody) != `export default "IMPORT-WAS-ALLOWED";` || imports.Load() != 1 {
		t.Fatalf("network import positive control failed: %s, %v", positiveBody, readErr)
	}
	imports.Store(0)
	environment := CredentialFreeEnvironment(os.Environ(), root)
	arguments, err := nativeServerArguments(hostSpec{Cwd: root, Socket: filepath.Join(root, "unused.sock"), BrokerTools: true}, environment)
	if err != nil {
		t.Fatal(err)
	}
	arguments[len(arguments)-1] = "stdio://"
	arguments = append(arguments, "--enable", "code_mode_only", "--config", `model_provider="probe"`, "--config", fmt.Sprintf(`model_providers.probe={name="local no-model dispatch",base_url=%q,wire_api="responses",requires_openai_auth=false,request_max_retries=0}`, server.URL))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "codex")
	if command.Path != binary {
		t.Fatal("native dispatch binary changed after discovery")
	}
	command.Args = append(command.Args, arguments...)
	command.Env, command.Dir, command.Stderr = environment, root, os.Stderr
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
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
				t.Fatal("invalid native dispatch response")
			}
			if string(response.ID) == fmt.Sprint(nextID) {
				if len(response.Error) > 0 {
					t.Fatalf("native dispatch %s: %s", method, response.Error)
				}
				return response.Result
			}
		}
		t.Fatalf("native dispatch %s ended before reply: %v", method, scanner.Err())
		return nil
	}
	call("initialize", map[string]any{"clientInfo": map[string]string{"name": "forgectl-goal-probe", "version": "dev"}, "capabilities": map[string]bool{"experimentalApi": true}})
	thread := call("thread/start", map[string]any{"cwd": root, "model": "gpt-6.1-sol", "modelProvider": "probe", "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": true, "allowProviderModelFallback": false, "dynamicTools": dynamicGoalTools()})
	var started struct {
		Thread struct{ ID string } `json:"thread"`
	}
	if json.Unmarshal(thread, &started) != nil || started.Thread.ID == "" {
		t.Fatal("native dispatch thread has no identity")
	}
	call("turn/start", map[string]any{"threadId": started.Thread.ID, "input": []map[string]string{{"type": "text", "text": "local no-model code-mode dispatch probe"}}})
	completed := false
	for scanner.Scan() {
		var event rpcEnvelope
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			t.Fatal("invalid native dispatch event")
		}
		if event.Method == "item/tool/call" {
			var request struct {
				ThreadID, Tool string
				Arguments      json.RawMessage
			}
			if json.Unmarshal(event.Params, &request) != nil || request.ThreadID != started.Thread.ID || request.Tool != "goal_inbox" {
				t.Fatalf("unexpected native tool request: %s", event.Params)
			}
			result, err := goaltools.Call(ctx, socket, capability, request.Tool, request.Arguments)
			if err != nil {
				t.Fatal(err)
			}
			if err := encoder.Encode(map[string]any{"id": event.ID, "result": map[string]any{"success": true, "contentItems": []map[string]string{{"type": "inputText", "text": string(result)}}}}); err != nil {
				t.Fatal(err)
			}
		}
		if event.Method == "turn/completed" {
			completed = true
			break
		}
	}
	if !completed || brokerCalls.Load() != 1 || requestCount.Load() != 2 || imports.Load() != 0 {
		t.Fatalf("native code-mode dispatch incomplete: completed=%v brokerCalls=%d requests=%d imports=%d", completed, brokerCalls.Load(), requestCount.Load(), imports.Load())
	}
	first, second := <-requests, <-requests
	var inventory struct{ Tools []struct{ Name string } }
	if json.Unmarshal(first, &inventory) != nil {
		t.Fatal("invalid native tool inventory")
	}
	var names []string
	for _, tool := range inventory.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"exec", "request_user_input", "wait"}) {
		t.Fatalf("unexpected code-mode tools: %v", names)
	}
	var followup struct {
		Input []struct {
			Type   string
			Output json.RawMessage
		}
	}
	if json.Unmarshal(second, &followup) != nil {
		t.Fatal("invalid native code-mode followup")
	}
	var toolOutput string
	for _, item := range followup.Input {
		if item.Type == "custom_tool_call_output" {
			toolOutput += string(item.Output)
		}
	}
	if !strings.Contains(toolOutput, "ISOLATE-BOUNDS-VERIFIED") || !strings.Contains(toolOutput, "BROKER-REACHED") {
		t.Fatalf("native code-mode isolation or broker output missing: %s", toolOutput)
	}
}
