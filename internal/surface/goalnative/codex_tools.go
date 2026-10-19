package goalnative

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/goaltools"
)

var brokerDisabledFeatures = []string{
	"shell_tool", "unified_exec", "shell_snapshot", "code_mode",
	"computer_use", "browser_use", "browser_use_external", "browser_use_full_cdp_access",
	"image_generation", "view_image", "in_app_browser", "in_app_local_automation",
	"skill_search", "skill_mcp_dependency_install", "workspace_dependencies", "remote_plugin",
	"tool_suggest", "request_permissions_tool", "multi_agent_v2", "artifact",
}

func dynamicGoalTools() []map[string]any {
	definitions := goaltools.Definitions()
	for _, definition := range definitions {
		definition["type"] = "function"
		definition["deferLoading"] = false
	}
	return definitions
}

func assertBrokerTools(raw json.RawMessage) error {
	var response struct {
		Config struct {
			Features map[string]json.RawMessage `json:"features"`
		} `json:"config"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return errors.New("native goal-tool configuration is malformed")
	}
	var dispatcher bool
	if json.Unmarshal(response.Config.Features["code_mode_host"], &dispatcher) != nil || !dispatcher {
		return errors.New("native goal-tool code-mode dispatcher is unavailable")
	}
	for _, feature := range brokerDisabledFeatures {
		if !nativeConfigFalse(response.Config.Features[feature]) {
			return errors.New("native built-in capability was not disabled: " + feature)
		}
	}
	return nil
}

func (session *Session) handleGoalTool(ctx context.Context, event rpcEnvelope) {
	respond := func(result json.RawMessage, err error) {
		text := string(result)
		if err != nil {
			text = err.Error()
		}
		response := map[string]any{"success": err == nil, "contentItems": []map[string]string{{"type": "inputText", "text": text}}}
		_ = session.send(ctx, rpcEnvelope{JSONRPC: protocolVersion, ID: event.ID, Result: mustJSON(response)})
	}
	select {
	case session.toolSlots <- struct{}{}:
	default:
		respond(nil, errors.New("native goal tool concurrency limit reached"))
		return
	}
	go func() {
		defer func() { <-session.toolSlots }()
		toolContext, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		go func() {
			select {
			case <-session.done:
				cancel()
			case <-toolContext.Done():
			}
		}()
		result, err := session.callGoalTool(toolContext, event.Params)
		respond(result, err)
	}()
}

func (session *Session) callGoalTool(ctx context.Context, parameters json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Arguments json.RawMessage `json:"arguments"`
		CallID    string          `json:"callId"`
		Namespace *string         `json:"namespace"`
		ThreadID  string          `json:"threadId"`
		Tool      string          `json:"tool"`
		TurnID    string          `json:"turnId"`
	}
	if goaltools.Decode(parameters, &request) != nil || request.ThreadID != session.ThreadID || request.CallID == "" || len(request.CallID) > 256 || request.Namespace != nil || !goaltools.KnownTool(request.Tool) {
		return nil, errors.New("invalid native goal tool request")
	}
	session.turnMu.Lock()
	_, active := session.nativeBusy[request.TurnID]
	session.turnMu.Unlock()
	if !active || session.closed.Load() {
		return nil, errors.New("native goal tool does not belong to an active turn")
	}
	session.mu.Lock()
	_, duplicate := session.toolCalls[request.CallID]
	if duplicate || len(session.toolCalls) >= 4096 {
		session.mu.Unlock()
		return nil, errors.New("native goal tool call repeated or exceeded session limit")
	}
	session.toolCalls[request.CallID] = struct{}{}
	session.mu.Unlock()
	capability, err := goaltools.ReadCapability(session.config.ToolCapabilityFile)
	if err != nil {
		return nil, err
	}
	return goaltools.Call(ctx, session.config.ToolSocket, capability, request.Tool, request.Arguments)
}
