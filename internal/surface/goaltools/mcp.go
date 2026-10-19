package goaltools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

func KnownTool(name string) bool {
	switch name {
	case "goal_exec", "goal_read", "goal_write", "goal_mail", "goal_inbox":
		return true
	default:
		return false
	}
}

func Definitions() []map[string]any {
	stringProperty := map[string]any{"type": "string"}
	definitions := []struct {
		name        string
		description string
		properties  map[string]any
		required    []string
	}{
		{"goal_exec", "Run argv in this role's fixed workspace sandbox. No network, source checkout, Git metadata or supervisor writes. Shell changes are checked against the approved paths at result time.", map[string]any{"argv": map[string]any{"type": "array", "items": stringProperty, "minItems": 1, "maxItems": 64}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 120}}, []string{"argv"}},
		{"goal_read", "Read a bounded regular source file, or the exact frozen patch supplied by the supervisor. Paths are relative to the isolated worktree; protected metadata and unsafe links are refused.", map[string]any{"path": stringProperty}, []string{"path"}},
		{"goal_write", "Write a UTF-8 regular source file within approved literal paths. Implementation only. Parent directories must already exist; metadata, symlinks and hard links are refused.", map[string]any{"path": stringProperty, "content": stringProperty}, []string{"path", "content"}},
		{"goal_mail", "Queue one protocol message to a named teammate. Queued is not completion; end the current turn and never resend it.", map[string]any{"to": stringProperty, "body": stringProperty}, []string{"to", "body"}},
		{"goal_inbox", "Read durable messages addressed to this exact native role incarnation.", map[string]any{}, []string{}},
	}
	result := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, map[string]any{"name": definition.name, "description": definition.description, "inputSchema": map[string]any{"type": "object", "properties": definition.properties, "required": definition.required, "additionalProperties": false}})
	}
	return result
}

func RunMCP(ctx context.Context, input io.Reader, output io.Writer, socket, capabilityFile string) error {
	capability, err := ReadCapability(capabilityFile)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 8192), maxFrame+1)
	encoder := termsafe.JSONEncoder(output)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := Decode(scanner.Bytes(), &request); err != nil || request.JSONRPC != "2.0" {
			return errors.New("invalid MCP request")
		}
		if len(request.ID) == 0 {
			if request.Method != "notifications/initialized" && request.Method != "notifications/cancelled" {
				return errors.New("unsupported MCP notification")
			}
			continue
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		switch request.Method {
		case "initialize":
			response["result"] = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "forgectl-goal-tools", "version": "1"}}
		case "ping":
			response["result"] = map[string]any{}
		case "tools/list":
			response["result"] = map[string]any{"tools": Definitions()}
		case "tools/call":
			var call struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
				Meta      json.RawMessage `json:"_meta,omitempty"`
			}
			if err := Decode(request.Params, &call); err != nil || !KnownTool(call.Name) {
				response["error"] = map[string]any{"code": -32602, "message": "invalid goal tool call"}
				break
			}
			result, err := Call(ctx, socket, capability, call.Name, call.Arguments)
			text := string(result)
			if err != nil {
				text = err.Error()
			}
			response["result"] = map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": err != nil}
		default:
			response["error"] = map[string]any{"code": -32601, "message": "MCP method unavailable in a repository goal"}
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}
