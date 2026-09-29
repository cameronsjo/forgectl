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
				out, _ := json.Marshal(respond(req))
				_, _ = c.Write(append(out, '\n'))
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return path, got
}

func TestPiDeliverAndState(t *testing.T) {
	idle := false
	path, got := startPi(t, shortTempDir(t), func(req piRequest) piResponse {
		switch req.Type {
		case "deliver":
			if req.Text == "" {
				return piResponse{OK: false, Error: "empty"}
			}
			return piResponse{OK: true, As: "steer"}
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
	if detail != "pi took it as steer" {
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
