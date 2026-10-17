package mail

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/hermes"
)

type HermesAdapter struct {
	Timeout time.Duration
}

type hermesRequest struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Text      string `json:"text,omitempty"`
}

type hermesResponse struct {
	V           int                `json:"v"`
	OK          bool               `json:"ok"`
	ID          string             `json:"id,omitempty"`
	RunID       string             `json:"run_id,omitempty"`
	SessionID   string             `json:"session_id,omitempty"`
	Accepted    *bool              `json:"accepted,omitempty"`
	Evidence    string             `json:"evidence,omitempty"`
	Idle        *bool              `json:"idle,omitempty"`
	Known       bool               `json:"known,omitempty"`
	Unconfirmed bool               `json:"unconfirmed,omitempty"`
	Retry       bool               `json:"retry,omitempty"`
	Error       string             `json:"error,omitempty"`
	Inspection  *hermes.Inspection `json:"inspection,omitempty"`
}

func (adapter HermesAdapter) Deliver(ctx context.Context, worker Worker, message Message, text string) (string, error) {
	response, err := adapter.call(ctx, worker, hermesRequest{V: 1, Type: "deliver", ID: message.ID, RunID: worker.RunID, SessionID: worker.ThreadID, Text: text})
	if err != nil {
		return "", err
	}
	if response.ID != message.ID {
		return "", Unconfirmed("hermes answered for message %s, not %s", quoteTrunc(response.ID), quoteTrunc(message.ID))
	}
	if response.Retry && (response.OK || response.Accepted != nil || response.Unconfirmed || response.Known || response.Evidence != "") {
		return "", Unconfirmed("hermes returned conflicting delivery state for %s", quoteTrunc(message.ID))
	}
	if response.Retry {
		return "", NotReady("hermes could not take it yet: %s", oneLine([]byte(response.Error), 200))
	}
	if response.Unconfirmed {
		return "", Unconfirmed("hermes acceptance is pending for %s: %s", quoteTrunc(message.ID), oneLine([]byte(response.Error), 200))
	}
	if !response.OK {
		if response.Accepted != nil || response.Known || response.Evidence != "" {
			return "", Unconfirmed("hermes returned conflicting acceptance evidence for %s", quoteTrunc(message.ID))
		}
		return "", fmt.Errorf("hermes refused it: %s", oneLine([]byte(response.Error), 200))
	}
	if response.Accepted == nil || !*response.Accepted || response.Evidence != "history" {
		return "", Unconfirmed("hermes has no native acceptance evidence for %s", quoteTrunc(message.ID))
	}
	return "hermes accepted message for native processing", nil
}

func (adapter HermesAdapter) Receipt(ctx context.Context, worker Worker, message Message) (string, bool, error) {
	response, err := adapter.call(ctx, worker, hermesRequest{V: 1, Type: "receipt", ID: message.ID, RunID: worker.RunID, SessionID: worker.ThreadID})
	if err != nil {
		return "", false, err
	}
	if response.ID != message.ID {
		return "", false, Unconfirmed("hermes receipt answered for message %s, not %s", quoteTrunc(response.ID), quoteTrunc(message.ID))
	}
	if response.Unconfirmed || response.Retry {
		return "", false, Unconfirmed("hermes receipt remains uncertain for %s", quoteTrunc(message.ID))
	}
	if response.OK && response.Known && response.Accepted != nil && *response.Accepted {
		if response.Evidence != "history" {
			return "", false, Unconfirmed("hermes receipt has no history evidence for %s", quoteTrunc(message.ID))
		}
		return "hermes receipt confirms native history acceptance", true, nil
	}
	return "hermes has no confirmed receipt", false, nil
}

func (adapter HermesAdapter) State(ctx context.Context, worker Worker) (WorkerState, error) {
	response, err := adapter.call(ctx, worker, hermesRequest{V: 1, Type: "state", RunID: worker.RunID, SessionID: worker.ThreadID})
	if err != nil {
		if IsRetryable(err) {
			return StateAbsent, nil
		}
		return StateUnknown, err
	}
	if !response.OK || response.Idle == nil {
		return StateUnknown, nil
	}
	if *response.Idle {
		return StateIdle, nil
	}
	return StateBusy, nil
}

func (adapter HermesAdapter) call(ctx context.Context, worker Worker, request hermesRequest) (hermesResponse, error) {
	if worker.Socket == "" || !filepath.IsAbs(worker.Socket) {
		return hermesResponse{}, NotReady("no hermes supervisor socket recorded for this worker")
	}
	if worker.RunID == "" || worker.ThreadID == "" {
		return hermesResponse{}, NotReady("hermes worker is missing its run or session identity")
	}
	timeout := adapter.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	// termsafe:allow-raw-json native request sent only to the Hermes supervisor Unix socket
	line, err := json.Marshal(request)
	if err != nil {
		return hermesResponse{}, err
	}
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", worker.Socket)
	if err != nil {
		return hermesResponse{}, NotReady("dial hermes supervisor: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return hermesResponse{}, Unconfirmed("hermes supervisor write outcome is unknown: %v", err)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	if !scanner.Scan() {
		if scanner.Err() != nil {
			return hermesResponse{}, Unconfirmed("hermes supervisor response outcome is unknown: %v", scanner.Err())
		}
		return hermesResponse{}, Unconfirmed("hermes supervisor closed without answering")
	}
	var response hermesResponse
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		return hermesResponse{}, Unconfirmed("hermes supervisor answered with invalid JSON")
	}
	if response.V != 1 {
		return hermesResponse{}, Unconfirmed("hermes supervisor answered with unsupported protocol version")
	}
	if response.RunID != worker.RunID || response.SessionID != worker.ThreadID {
		return hermesResponse{}, Unconfirmed("hermes response identity does not match worker registration")
	}
	return response, nil
}
