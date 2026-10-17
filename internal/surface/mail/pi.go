package mail

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"time"
)

// PiAdapter talks to the forgectl pi extension (assets/forgectl-inbox.ts)
// over the unix socket forgectl hands it at launch in FORGECTL_INBOX. One
// request per connection: a JSON line in, a JSON line back.
type PiAdapter struct {
	// Timeout bounds one request. Zero is 3s.
	Timeout time.Duration
}

type piRequest struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Text      string `json:"text,omitempty"`
	Priority  string `json:"priority,omitempty"`
}

type piResponse struct {
	V           int    `json:"v,omitempty"`
	OK          bool   `json:"ok"`
	ID          string `json:"id,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	As          string `json:"as,omitempty"`
	Accepted    *bool  `json:"accepted,omitempty"`
	Evidence    string `json:"evidence,omitempty"`
	Idle        *bool  `json:"idle,omitempty"`
	Known       bool   `json:"known,omitempty"`
	Unconfirmed bool   `json:"unconfirmed,omitempty"`
	Error       string `json:"error,omitempty"`
	// Retry marks a refusal pi may not repeat, such as a send that raced
	// the start or end of a run; the message stays queued.
	Retry bool `json:"retry,omitempty"`
}

const piIdentityProtocol = 2

func piRequestFor(worker Worker, request piRequest) piRequest {
	if worker.RunID == "" && worker.ThreadID == "" {
		request.V = 1
		return request
	}
	request.V = piIdentityProtocol
	request.RunID = worker.RunID
	request.SessionID = worker.ThreadID
	return request
}

func piDefiniteRefusal(response piResponse) bool {
	if response.OK || response.Unconfirmed || response.Retry || response.Evidence != "" {
		return false
	}
	return response.Accepted == nil || !*response.Accepted
}

// Deliver hands text to the extension, which prompts when pi is idle, steers
// when it is busy, and queues a follow-up for PriorityLater.
func (a PiAdapter) Deliver(ctx context.Context, w Worker, m Message, text string) (string, error) {
	resp, err := a.call(ctx, w, piRequestFor(w, piRequest{Type: "deliver", ID: m.ID, Text: text, Priority: string(m.Priority)}), true)
	if err != nil {
		return "", err
	}
	if resp.ID != m.ID {
		return "", Unconfirmed("pi inbox answered for message %s, not %s", quoteTrunc(resp.ID), quoteTrunc(m.ID))
	}
	if resp.Unconfirmed {
		return "", Unconfirmed("pi has not confirmed message %s: %s", quoteTrunc(m.ID), oneLine([]byte(resp.Error), 200))
	}
	if !resp.OK && resp.Retry {
		return "", NotReady("pi could not take it yet: %s", oneLine([]byte(resp.Error), 200))
	}
	if !resp.OK {
		return "", fmt.Errorf("pi extension refused it: %s", oneLine([]byte(resp.Error), 200))
	}
	if resp.Accepted == nil || !*resp.Accepted || (resp.Evidence != "message_start" && resp.Evidence != "history") {
		return "", Unconfirmed("pi has no native-consumption evidence for message %s", quoteTrunc(m.ID))
	}
	if resp.SessionID == "" {
		return "", Unconfirmed("pi accepted message %s without a session identity", quoteTrunc(m.ID))
	}
	as := resp.As
	if as == "" {
		as = "prompt"
	}
	return "pi native consumption observed via " + oneLine([]byte(resp.Evidence), 20) + " as " + oneLine([]byte(as), 20) + "; transcript durability/completion is not confirmed", nil
}

// Receipt asks pi whether it observed native consumption of a message ID. An
// accepted receipt does not prove a completed model turn, and message_start
// precedes transcript persistence.
func (a PiAdapter) Receipt(ctx context.Context, w Worker, m Message) (string, bool, error) {
	resp, err := a.call(ctx, w, piRequestFor(w, piRequest{Type: "receipt", ID: m.ID}), true)
	if err != nil {
		return "", false, err
	}
	if resp.ID != m.ID {
		return "", false, Unconfirmed("pi receipt answered for message %s, not %s", quoteTrunc(resp.ID), quoteTrunc(m.ID))
	}
	if resp.OK && resp.Accepted != nil && resp.Known {
		if !*resp.Accepted {
			return "pi receipt confirms rejection: " + oneLine([]byte(resp.Error), 200), false, nil
		}
		if resp.Evidence != "message_start" && resp.Evidence != "history" {
			return "", false, Unconfirmed("pi receipt for %s has no native-consumption evidence", quoteTrunc(m.ID))
		}
		if resp.SessionID == "" {
			return "", false, Unconfirmed("pi receipt accepted %s without a session identity", quoteTrunc(m.ID))
		}
		return "pi receipt confirms native consumption via " + resp.Evidence + "; transcript durability/completion is not confirmed", true, nil
	}
	if resp.Unconfirmed {
		return "", false, Unconfirmed("pi receipt for %s is unconfirmed: %s", quoteTrunc(m.ID), oneLine([]byte(resp.Error), 200))
	}
	return "", false, Unconfirmed("pi has no confirmed receipt for %s", quoteTrunc(m.ID))
}

// State asks the extension whether pi is idle.
func (a PiAdapter) State(ctx context.Context, w Worker) (WorkerState, error) {
	resp, err := a.call(ctx, w, piRequestFor(w, piRequest{Type: "state"}), false)
	if err != nil {
		if IsRetryable(err) {
			return StateAbsent, nil
		}
		return StateUnknown, err
	}
	if !resp.OK || resp.Idle == nil {
		return StateUnknown, nil
	}
	if *resp.Idle {
		return StateIdle, nil
	}
	return StateBusy, nil
}

func (a PiAdapter) call(ctx context.Context, worker Worker, req piRequest, uncertain bool) (piResponse, error) {
	if worker.Socket == "" || !filepath.IsAbs(worker.Socket) {
		return piResponse{}, NotReady("no pi inbox socket recorded for this worker")
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	// termsafe:allow-raw-json a request line on the pi extension socket, never rendered
	line, err := json.Marshal(req)
	if err != nil {
		return piResponse{}, err
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "unix", worker.Socket)
	if err != nil {
		return piResponse{}, NotReady("dial pi inbox %s: %v", worker.Socket, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(append(line, '\n')); err != nil {
		if uncertain {
			return piResponse{}, Unconfirmed("pi inbox write outcome is unknown: %v", err)
		}
		return piResponse{}, NotReady("write pi inbox: %v", err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			if uncertain {
				return piResponse{}, Unconfirmed("pi inbox response outcome is unknown: %v", err)
			}
			return piResponse{}, NotReady("read pi inbox: %v", err)
		}
		if uncertain {
			return piResponse{}, Unconfirmed("pi inbox closed without answering")
		}
		return piResponse{}, NotReady("pi inbox closed without answering")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(sc.Bytes(), &fields); err != nil {
		if uncertain {
			return piResponse{}, Unconfirmed("pi inbox answered with something that is not JSON")
		}
		return piResponse{}, errors.New("pi inbox answered with something that is not JSON")
	}
	if _, ok := fields["ok"]; !ok {
		if uncertain {
			return piResponse{}, Unconfirmed("pi inbox answered without an ok field")
		}
		return piResponse{}, errors.New("pi inbox answered without an ok field")
	}
	var resp piResponse
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		if uncertain {
			return piResponse{}, Unconfirmed("pi inbox answered with an invalid response")
		}
		return piResponse{}, errors.New("pi inbox answered with an invalid response")
	}
	if req.V == piIdentityProtocol {
		if resp.V != piIdentityProtocol {
			if piDefiniteRefusal(resp) {
				return resp, fmt.Errorf("pi inbox refused protocol v%d: %s", piIdentityProtocol, oneLine([]byte(resp.Error), 200))
			}
			return resp, Unconfirmed("pi inbox answered with an unsupported protocol version")
		}
		if resp.RunID != worker.RunID || (worker.ThreadID != "" && resp.SessionID != worker.ThreadID) {
			if piDefiniteRefusal(resp) {
				return resp, fmt.Errorf("pi inbox refused worker identity: %s", oneLine([]byte(resp.Error), 200))
			}
			return resp, Unconfirmed("pi inbox response identity does not match worker registration")
		}
	} else if resp.V != 0 && resp.V != 1 {
		return resp, Unconfirmed("pi inbox answered with an unsupported protocol version")
	}
	return resp, nil
}
