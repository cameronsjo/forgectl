//go:build unix

package mail

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func startPi(t *testing.T, dir string, respond func(piRequest) piResponse) (string, chan piRequest) {
	t.Helper()
	path := filepath.Join(dir, "pi.sock")
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan piRequest, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				sc := bufio.NewScanner(c)
				if !sc.Scan() {
					return
				}
				var req piRequest
				if json.Unmarshal(sc.Bytes(), &req) != nil {
					return
				}
				got <- req
				response := respond(req)
				if response.ID == "" {
					response.ID = req.ID
				}
				out, _ := json.Marshal(response)
				_, _ = c.Write(append(out, '\n'))
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return path, got
}

// A send pi rejects while between runs stays queued instead of failing.
func TestPiDeliverRetryIsNotReady(t *testing.T) {
	path, _ := startPi(t, shortTempDir(t), func(piRequest) piResponse {
		return piResponse{OK: false, Retry: true, Error: "agent is not streaming"}
	})
	a := PiAdapter{Timeout: 2 * time.Second}
	_, err := a.Deliver(context.Background(), Worker{Name: "pi-1", Harness: HarnessPi, Socket: path}, Message{ID: "m-1", Priority: PriorityNext}, "x")
	if !IsRetryable(err) {
		t.Fatalf("err %v, want retryable", err)
	}
}

func TestPiDeliverLostAckIsUnconfirmed(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "pi.sock")
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan piRequest, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		sc := bufio.NewScanner(conn)
		if !sc.Scan() {
			return
		}
		var req piRequest
		if json.Unmarshal(sc.Bytes(), &req) == nil {
			got <- req
		}
	}()

	a := PiAdapter{Timeout: time.Second}
	_, err = a.Deliver(context.Background(), Worker{Name: "pi-1", Harness: HarnessPi, Socket: path}, Message{ID: "m-lost"}, "x")
	if !IsUnconfirmed(err) {
		t.Fatalf("err = %v, want unconfirmed", err)
	}
	if req := <-got; req.ID != "m-lost" {
		t.Fatalf("request id = %q, want m-lost", req.ID)
	}
}

func TestPiDeliverAndState(t *testing.T) {
	idle := false
	path, got := startPi(t, shortTempDir(t), func(req piRequest) piResponse {
		switch req.Type {
		case "deliver":
			if req.Text == "" {
				return piResponse{OK: false, Error: "empty"}
			}
			accepted := true
			return piResponse{OK: true, Accepted: &accepted, Evidence: "message_start", SessionID: "session-1", As: "steer"}
		case "state":
			return piResponse{OK: true, Idle: &idle}
		}
		return piResponse{OK: false, Error: "unknown request"}
	})
	a := PiAdapter{Timeout: 2 * time.Second}
	w := Worker{Name: "pi-1", Harness: HarnessPi, Socket: path}
	ctx := context.Background()

	detail, err := a.Deliver(ctx, w, Message{ID: "m-1", Priority: PriorityNext}, "look at x")
	if err != nil {
		t.Fatal(err)
	}
	if detail != "pi native consumption observed via message_start as steer; transcript durability/completion is not confirmed" {
		t.Errorf("detail %q", detail)
	}
	req := <-got
	if req.V != 1 || req.Type != "deliver" || req.ID != "m-1" || req.Text != "look at x" || req.Priority != "next" {
		t.Fatalf("request %+v", req)
	}

	if _, err := a.Deliver(ctx, w, Message{ID: "m-2"}, ""); err == nil || IsRetryable(err) {
		t.Fatalf("refusal: err = %v, want a permanent error", err)
	}
	<-got

	st, err := a.State(ctx, w)
	if err != nil || st != StateBusy {
		t.Fatalf("state = %q, %v; want busy", st, err)
	}
}

func TestPiDeliverWithoutNativeEvidenceStaysUnconfirmed(t *testing.T) {
	path, _ := startPi(t, shortTempDir(t), func(req piRequest) piResponse {
		return piResponse{OK: true, As: "prompt"}
	})
	a := PiAdapter{Timeout: 2 * time.Second}
	_, err := a.Deliver(context.Background(), Worker{Name: "pi-1", Harness: HarnessPi, Socket: path}, Message{ID: "m-no-native"}, "x")
	if !IsUnconfirmed(err) {
		t.Fatalf("err = %v, want unconfirmed", err)
	}
}

func TestPiAbsent(t *testing.T) {
	a := PiAdapter{Timeout: time.Second}
	ctx := context.Background()
	if _, err := a.Deliver(ctx, Worker{Name: "pi-1"}, Message{}, "x"); !IsRetryable(err) {
		t.Fatalf("no socket recorded: err = %v, want retryable", err)
	}
	gone := Worker{Name: "pi-1", Socket: filepath.Join(shortTempDir(t), "gone.sock")}
	if _, err := a.Deliver(ctx, gone, Message{}, "x"); !IsRetryable(err) {
		t.Fatalf("pi not started: err = %v, want retryable", err)
	}
	if st, _ := a.State(ctx, gone); st != StateAbsent {
		t.Fatalf("state = %q, want absent", st)
	}
}

func TestPiReceiptConfirmsAPIAcceptance(t *testing.T) {
	path, got := startPi(t, shortTempDir(t), func(req piRequest) piResponse {
		if req.Type != "receipt" {
			return piResponse{OK: false, Error: "unexpected request"}
		}
		accepted := true
		return piResponse{OK: true, Accepted: &accepted, Evidence: "message_start", SessionID: "session-1", Known: true}
	})
	a := PiAdapter{Timeout: 2 * time.Second}
	detail, accepted, err := a.Receipt(context.Background(), Worker{Name: "pi-1", Harness: HarnessPi, Socket: path}, Message{ID: "m-receipt"})
	if err != nil || !accepted {
		t.Fatalf("receipt = %q, %v, accepted=%v", detail, err, accepted)
	}
	if req := <-got; req.Type != "receipt" || req.ID != "m-receipt" {
		t.Fatalf("request = %+v", req)
	}
	if detail != "pi receipt confirms native consumption via message_start; transcript durability/completion is not confirmed" {
		t.Fatalf("detail = %q", detail)
	}
}

func TestPiReceiptUnknownStaysUnconfirmed(t *testing.T) {
	path, _ := startPi(t, shortTempDir(t), func(req piRequest) piResponse {
		return piResponse{OK: false, Unconfirmed: true, Error: "receipt unknown"}
	})
	a := PiAdapter{Timeout: 2 * time.Second}
	_, accepted, err := a.Receipt(context.Background(), Worker{Name: "pi-1", Harness: HarnessPi, Socket: path}, Message{ID: "m-unknown"})
	if accepted {
		t.Fatal("unknown receipt reported accepted")
	}
	if !IsUnconfirmed(err) {
		t.Fatalf("err = %v, want unconfirmed", err)
	}
}

func TestPiReceiptWithoutReceiverEvidenceStaysUnconfirmed(t *testing.T) {
	path, _ := startPi(t, shortTempDir(t), func(req piRequest) piResponse {
		accepted := true
		return piResponse{OK: true, Accepted: &accepted}
	})
	a := PiAdapter{Timeout: 2 * time.Second}
	_, accepted, err := a.Receipt(context.Background(), Worker{Name: "pi-1", Harness: HarnessPi, Socket: path}, Message{ID: "m-no-evidence"})
	if accepted {
		t.Fatal("receipt without receiver evidence reported accepted")
	}
	if !IsUnconfirmed(err) {
		t.Fatalf("err = %v, want unconfirmed", err)
	}
}

func TestPiReceiptKnownRejectionDoesNotNeedAcceptanceEvidence(t *testing.T) {
	path, _ := startPi(t, shortTempDir(t), func(req piRequest) piResponse {
		accepted := false
		return piResponse{OK: true, Accepted: &accepted, Known: true, Error: "pi rejected the message"}
	})
	a := PiAdapter{Timeout: 2 * time.Second}
	detail, accepted, err := a.Receipt(context.Background(), Worker{Name: "pi-1", Harness: HarnessPi, Socket: path}, Message{ID: "m-rejected"})
	if err != nil || accepted {
		t.Fatalf("receipt = %q, %v, accepted=%v", detail, err, accepted)
	}
}

func TestPiDeliverWithoutSessionIdentityStaysUnconfirmed(t *testing.T) {
	path, _ := startPi(t, shortTempDir(t), func(req piRequest) piResponse {
		accepted := true
		return piResponse{OK: true, Accepted: &accepted, Evidence: "message_start"}
	})
	a := PiAdapter{Timeout: 2 * time.Second}
	_, err := a.Deliver(context.Background(), Worker{Name: "pi-1", Harness: HarnessPi, Socket: path}, Message{ID: "m-no-session"}, "x")
	if !IsUnconfirmed(err) {
		t.Fatalf("err = %v, want unconfirmed", err)
	}
}

func TestPiReceiptWithoutSessionIdentityStaysUnconfirmed(t *testing.T) {
	path, _ := startPi(t, shortTempDir(t), func(req piRequest) piResponse {
		accepted := true
		return piResponse{OK: true, Accepted: &accepted, Evidence: "message_start", Known: true}
	})
	a := PiAdapter{Timeout: 2 * time.Second}
	_, accepted, err := a.Receipt(context.Background(), Worker{Name: "pi-1", Harness: HarnessPi, Socket: path}, Message{ID: "m-no-session"})
	if accepted || !IsUnconfirmed(err) {
		t.Fatalf("receipt accepted=%v, err=%v; want unconfirmed", accepted, err)
	}
}

func TestPiResponseForAnotherMessageStaysUnconfirmed(t *testing.T) {
	path, _ := startPi(t, shortTempDir(t), func(req piRequest) piResponse {
		return piResponse{OK: true, ID: "other", As: "steer"}
	})
	a := PiAdapter{Timeout: 2 * time.Second}
	_, err := a.Deliver(context.Background(), Worker{Name: "pi-1", Harness: HarnessPi, Socket: path}, Message{ID: "m-expected"}, "x")
	if !IsUnconfirmed(err) {
		t.Fatalf("err = %v, want unconfirmed", err)
	}
}
