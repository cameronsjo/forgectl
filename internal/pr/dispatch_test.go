package pr

import (
	"context"
	"errors"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

func dispatchWindowRow(pid, start, id, session, name string) string {
	return strings.Join([]string{pid, start, id, session, "1", name, "0", "1"}, "\x1f")
}

func TestVerifyDispatched_ExactGenerationSessionAndName(t *testing.T) {
	refLive := Ref{Owner: "o", Repo: "r", Number: 1}
	refRestarted := Ref{Owner: "o", Repo: "r", Number: 2}
	refDuplicate := Ref{Owner: "o", Repo: "r", Number: 3}
	rows := strings.Join([]string{
		dispatchWindowRow("10", "20", "@1", "reviews", windowName(refLive)),
		dispatchWindowRow("99", "20", "@0", "reviews", windowName(refRestarted)),
		dispatchWindowRow("10", "20", "@7", "reviews", windowName(refDuplicate)),
	}, "\n")
	fake := &internalexec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if args[0] == "list-windows" {
			return rows, nil
		}
		return "", nil
	}}
	waits := 0
	c := New(fake, WithTmuxSession("reviews"), WithDispatchWait(func(context.Context) error { waits++; return nil }))
	dispatches := []Dispatch{
		{Ref: refLive, WindowID: "10\x1f20\x1f@1"},
		{Ref: refRestarted, WindowID: "10\x1f20\x1f@0"},
		{Ref: refDuplicate, WindowID: "10\x1f20\x1f@7"},
		{Ref: refDuplicate, WindowID: "10\x1f20\x1f@7"},
	}
	gone, err := c.VerifyDispatched(context.Background(), dispatches)
	if err != nil {
		t.Fatalf("VerifyDispatched: %v", err)
	}
	if waits != 1 {
		t.Errorf("waits = %d, want 1", waits)
	}
	listCalls := 0
	for _, call := range fake.Calls {
		if len(call.Args) > 0 && call.Args[0] == "list-windows" {
			listCalls++
		}
	}
	if listCalls != 1 {
		t.Errorf("list calls = %d, want 1", listCalls)
	}
	if len(gone) != 1 || gone[0].Ref != refRestarted {
		t.Fatalf("gone = %+v, want restarted ref only", gone)
	}
}

func TestVerifyDispatched_EmptyDoesNotWaitOrList(t *testing.T) {
	fake := &internalexec.FakeRunner{}
	waits := 0
	c := New(fake, WithDispatchWait(func(context.Context) error { waits++; return nil }))
	gone, err := c.VerifyDispatched(context.Background(), nil)
	if err != nil || gone != nil {
		t.Fatalf("VerifyDispatched(nil) = (%v,%v), want nil,nil", gone, err)
	}
	if waits != 0 || len(fake.Calls) != 0 {
		t.Fatalf("waits=%d calls=%v, want zero", waits, fake.Calls)
	}
}

func TestVerifyDispatched_WaitAndListErrorsAreUnknown(t *testing.T) {
	waitErr := errors.New("wait canceled")
	c := New(&internalexec.FakeRunner{}, WithDispatchWait(func(context.Context) error { return waitErr }))
	gone, err := c.VerifyDispatched(context.Background(), []Dispatch{{Ref: Ref{Owner: "o", Repo: "r", Number: 1}, WindowID: "1\x1f2\x1f@3"}})
	if gone != nil || !errors.Is(err, waitErr) {
		t.Fatalf("wait result = (%v,%v), want nil wrapping sentinel", gone, err)
	}

	listErr := errors.New("list failed")
	fake := &internalexec.FakeRunner{RunFunc: func(string, []string) (string, error) { return "", listErr }}
	c = New(fake, WithDispatchWait(func(context.Context) error { return nil }))
	gone, err = c.VerifyDispatched(context.Background(), []Dispatch{{Ref: Ref{Owner: "o", Repo: "r", Number: 1}, WindowID: "1\x1f2\x1f@3"}})
	if gone != nil || !errors.Is(err, listErr) {
		t.Fatalf("list result = (%v,%v), want nil wrapping sentinel", gone, err)
	}
}
