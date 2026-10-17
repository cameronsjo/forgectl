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

func startHermes(test *testing.T, dir string, respond func(hermesRequest) hermesResponse) (string, chan hermesRequest) {
	test.Helper()
	path := filepath.Join(dir, "hermes.sock")
	listener, err := (&net.ListenConfig{}).Listen(test.Context(), "unix", path)
	if err != nil {
		test.Fatal(err)
	}
	received := make(chan hermesRequest, 8)
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func(connection net.Conn) {
				defer func() { _ = connection.Close() }()
				scanner := bufio.NewScanner(connection)
				if !scanner.Scan() {
					return
				}
				var request hermesRequest
				if json.Unmarshal(scanner.Bytes(), &request) != nil {
					return
				}
				received <- request
				response := respond(request)
				if response.ID == "" {
					response.ID = request.ID
				}
				if response.RunID == "" {
					response.RunID = request.RunID
				}
				if response.SessionID == "" {
					response.SessionID = request.SessionID
				}
				data, _ := json.Marshal(response)
				_, _ = connection.Write(append(data, '\n'))
			}(connection)
		}
	}()
	test.Cleanup(func() { _ = listener.Close() })
	return path, received
}

func hermesWorker(socket string) Worker {
	return Worker{Name: "hermes-1", Harness: HarnessHermes, Socket: socket, RunID: "run-1", ThreadID: "session-1"}
}

func TestHermesDeliverRequiresHistoryEvidence(test *testing.T) {
	accepted := true
	path, received := startHermes(test, shortTempDir(test), func(request hermesRequest) hermesResponse {
		return hermesResponse{V: 1, OK: true, Accepted: &accepted, Evidence: "history"}
	})
	detail, err := (HermesAdapter{Timeout: time.Second}).Deliver(context.Background(), hermesWorker(path), Message{ID: "m-1"}, "hello")
	if err != nil || detail == "" {
		test.Fatalf("deliver = %q, %v", detail, err)
	}
	request := <-received
	if request.V != 1 || request.Type != "deliver" || request.ID != "m-1" || request.RunID != "run-1" || request.SessionID != "session-1" || request.Text != "hello" {
		test.Fatalf("request = %+v", request)
	}
	path, _ = startHermes(test, shortTempDir(test), func(hermesRequest) hermesResponse {
		return hermesResponse{V: 1, OK: true, Accepted: &accepted, Evidence: "ack"}
	})
	if _, err := (HermesAdapter{Timeout: time.Second}).Deliver(context.Background(), hermesWorker(path), Message{ID: "m-2"}, "hello"); !IsUnconfirmed(err) {
		test.Fatalf("deliver without history = %v, want unconfirmed", err)
	}
}

func TestHermesReceiptAndState(test *testing.T) {
	accepted := true
	idle := true
	path, received := startHermes(test, shortTempDir(test), func(request hermesRequest) hermesResponse {
		switch request.Type {
		case "receipt":
			return hermesResponse{V: 1, OK: true, Known: true, Accepted: &accepted, Evidence: "history"}
		case "state":
			return hermesResponse{V: 1, OK: true, Idle: &idle}
		default:
			return hermesResponse{V: 1, Error: "unexpected request"}
		}
	})
	adapter := HermesAdapter{Timeout: time.Second}
	detail, confirmed, err := adapter.Receipt(context.Background(), hermesWorker(path), Message{ID: "m-1"})
	if err != nil || !confirmed || detail == "" {
		test.Fatalf("receipt = %q, %v, confirmed=%t", detail, err, confirmed)
	}
	state, err := adapter.State(context.Background(), hermesWorker(path))
	if err != nil || state != StateIdle {
		test.Fatalf("state = %q, %v", state, err)
	}
	for range 2 {
		request := <-received
		if request.RunID != "run-1" || request.SessionID != "session-1" {
			test.Fatalf("request identity = %+v", request)
		}
	}
}

func TestHermesMismatchedResponseIsUnconfirmed(test *testing.T) {
	accepted := true
	path, _ := startHermes(test, shortTempDir(test), func(hermesRequest) hermesResponse {
		return hermesResponse{V: 1, OK: true, Accepted: &accepted, Evidence: "history", RunID: "other-run"}
	})
	_, err := (HermesAdapter{Timeout: time.Second}).Deliver(context.Background(), hermesWorker(path), Message{ID: "m-1"}, "hello")
	if !IsUnconfirmed(err) {
		test.Fatalf("mismatched response = %v, want unconfirmed", err)
	}
}

func TestHermesConflictingRepliesRemainUnconfirmed(test *testing.T) {
	accepted := true
	for _, response := range []hermesResponse{
		{V: 1, OK: true},
		{V: 1, OK: true, Accepted: &accepted, Evidence: "message_start"},
		{V: 1, OK: true, Accepted: &accepted, Evidence: "history", Retry: true},
		{V: 1, OK: true, Accepted: &accepted, Evidence: "history", Unconfirmed: true},
		{V: 1, Accepted: &accepted, Evidence: "history"},
		{V: 0, OK: true, Accepted: &accepted, Evidence: "history"},
	} {
		path, _ := startHermes(test, shortTempDir(test), func(hermesRequest) hermesResponse { return response })
		if _, err := (HermesAdapter{Timeout: time.Second}).Deliver(test.Context(), hermesWorker(path), Message{ID: "message-1"}, "body"); !IsUnconfirmed(err) {
			test.Fatalf("ambiguous reply = %#v; err=%v", response, err)
		}
	}
	path, _ := startHermes(test, shortTempDir(test), func(hermesRequest) hermesResponse {
		return hermesResponse{V: 1, OK: true, Known: true, Accepted: &accepted, Evidence: "history", Unconfirmed: true}
	})
	_, confirmed, err := (HermesAdapter{Timeout: time.Second}).Receipt(test.Context(), hermesWorker(path), Message{ID: "message-1"})
	if confirmed || !IsUnconfirmed(err) {
		test.Fatalf("conflicting receipt confirmed=%t, err=%v", confirmed, err)
	}
}
