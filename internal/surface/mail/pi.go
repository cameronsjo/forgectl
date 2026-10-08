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
	V        int    `json:"v"`
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	Text     string `json:"text,omitempty"`
	Priority string `json:"priority,omitempty"`
}

type piResponse struct {
	OK    bool   `json:"ok"`
	As    string `json:"as,omitempty"`
	Idle  *bool  `json:"idle,omitempty"`
	Error string `json:"error,omitempty"`
	// Retry marks a refusal pi may not repeat, such as a send that raced
	// the start or end of a run; the message stays queued.
	Retry bool `json:"retry,omitempty"`
}

// Deliver hands text to the extension, which prompts when pi is idle, steers
// when it is busy, and queues a follow-up for PriorityLater.
func (a PiAdapter) Deliver(ctx context.Context, w Worker, m Message, text string) (string, error) {
	resp, err := a.call(ctx, w.Socket, piRequest{V: 1, Type: "deliver", ID: m.ID, Text: text, Priority: string(m.Priority)})
	if err != nil {
		return "", err
	}
	if !resp.OK && resp.Retry {
		return "", NotReady("pi could not take it yet: %s", oneLine([]byte(resp.Error), 200))
	}
	if !resp.OK {
		return "", fmt.Errorf("pi extension refused it: %s", oneLine([]byte(resp.Error), 200))
	}
	as := resp.As
	if as == "" {
		as = "prompt"
	}
	return "pi took it as " + oneLine([]byte(as), 20), nil
}

// State asks the extension whether pi is idle.
func (a PiAdapter) State(ctx context.Context, w Worker) (WorkerState, error) {
	resp, err := a.call(ctx, w.Socket, piRequest{V: 1, Type: "state"})
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

func (a PiAdapter) call(ctx context.Context, socket string, req piRequest) (piResponse, error) {
	if socket == "" || !filepath.IsAbs(socket) {
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
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return piResponse{}, NotReady("dial pi inbox %s: %v", socket, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return piResponse{}, NotReady("write pi inbox: %v", err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return piResponse{}, NotReady("read pi inbox: %v", err)
		}
		return piResponse{}, NotReady("pi inbox closed without answering")
	}
	var resp piResponse
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return piResponse{}, errors.New("pi inbox answered with something that is not JSON")
	}
	return resp, nil
}
