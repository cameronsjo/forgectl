package goaltools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunMCPInitializeListCallAndUnknownMethod(t *testing.T) {
	root := privateTestDir(t)
	socket := filepath.Join(root, "tools.sock")
	capabilityFile := filepath.Join(root, "capability")
	server, err := Start(context.Background(), socket, capabilityFile, func(_ context.Context, operation string, arguments json.RawMessage) (any, error) {
		if operation != "goal_read" {
			t.Fatalf("operation = %q", operation)
		}
		if string(arguments) != `{"path":"README.md"}` {
			t.Fatalf("arguments = %s", arguments)
		}
		return map[string]any{"path": "README.md", "content": "ok"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer(t, server)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"goal_read","arguments":{"path":"README.md"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"method/unknown","params":{}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := RunMCP(context.Background(), strings.NewReader(input), &output, socket, capabilityFile); err != nil {
		t.Fatal(err)
	}
	responses := decodeMCPResponses(t, output.Bytes())
	if len(responses) != 4 {
		t.Fatalf("response count = %d; output = %s", len(responses), output.Bytes())
	}
	if got := responses[0]["result"].(map[string]any)["protocolVersion"]; got != "2024-11-05" {
		t.Fatalf("protocol version = %v", got)
	}
	tools := responses[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 5 {
		t.Fatalf("tool count = %d", len(tools))
	}
	callResult := responses[2]["result"].(map[string]any)
	if callResult["isError"] != false {
		t.Fatalf("call result = %#v", callResult)
	}
	content := callResult["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" || !strings.Contains(content[0].(map[string]any)["text"].(string), "README.md") {
		t.Fatalf("call content = %#v", content)
	}
	unknownError := responses[3]["error"].(map[string]any)
	if unknownError["code"] != float64(-32601) {
		t.Fatalf("unknown method error = %#v", unknownError)
	}
}

func TestRunMCPRejectsMalformedOversizeAndUnknownProperties(t *testing.T) {
	root := privateTestDir(t)
	socket := filepath.Join(root, "tools.sock")
	capabilityFile := filepath.Join(root, "capability")
	server, err := Start(context.Background(), socket, capabilityFile, func(context.Context, string, json.RawMessage) (any, error) {
		return "unexpected", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer(t, server)
	cases := []struct {
		name  string
		input string
	}{
		{name: "malformed", input: "{\n"},
		{name: "oversize", input: strings.Repeat("x", maxFrame+1) + "\n"},
		{name: "unknown property", input: `{"jsonrpc":"2.0","id":1,"method":"ping","extra":true}` + "\n"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := RunMCP(context.Background(), strings.NewReader(testCase.input), &output, socket, capabilityFile); err == nil {
				t.Fatal("RunMCP accepted invalid input")
			}
			if output.Len() != 0 {
				t.Fatalf("output = %s", output.Bytes())
			}
		})
	}
}

func TestRunMCPRejectsUnknownToolCallWithoutBrokerDispatch(t *testing.T) {
	root := privateTestDir(t)
	socket := filepath.Join(root, "tools.sock")
	capabilityFile := filepath.Join(root, "capability")
	server, err := Start(context.Background(), socket, capabilityFile, func(context.Context, string, json.RawMessage) (any, error) {
		t.Fatal("unknown tool reached broker")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer(t, server)
	input := `{"jsonrpc":"2.0","id":"bad-tool","method":"tools/call","params":{"name":"goal_shell","arguments":{}}}` + "\n"
	var output bytes.Buffer
	if err := RunMCP(context.Background(), strings.NewReader(input), &output, socket, capabilityFile); err != nil {
		t.Fatal(err)
	}
	responses := decodeMCPResponses(t, output.Bytes())
	if len(responses) != 1 {
		t.Fatalf("responses = %#v", responses)
	}
	errResponse := responses[0]["error"].(map[string]any)
	if errResponse["code"] != float64(-32602) || errResponse["message"] != "invalid goal tool call" {
		t.Fatalf("error response = %#v", errResponse)
	}
}

func decodeMCPResponses(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var responses []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var response map[string]any
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		responses = append(responses, response)
	}
	return responses
}

func TestRunMCPUsesPrivateCapabilityFile(t *testing.T) {
	root := privateTestDir(t)
	capabilityFile := filepath.Join(root, "capability")
	if err := os.WriteFile(capabilityFile, []byte("not-a-capability"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := RunMCP(context.Background(), strings.NewReader("{}\n"), &output, filepath.Join(root, "tools.sock"), capabilityFile)
	if err == nil || !strings.Contains(err.Error(), "private 64-byte regular file") {
		t.Fatalf("error = %v", err)
	}
}
