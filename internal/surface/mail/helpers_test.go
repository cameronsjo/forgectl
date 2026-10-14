package mail

import (
	"context"
	"os"
	"testing"
	"time"
)

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time      { return c.t }
func (c *testClock) add(d time.Duration) { c.t = c.t.Add(d) }

// fakeAdapter returns errs in order, then succeeds.
type fakeAdapter struct {
	errs  []error
	calls int
	texts []string
	state WorkerState
}

func (f *fakeAdapter) Deliver(ctx context.Context, w Worker, m Message, text string) (string, error) {
	f.texts = append(f.texts, text)
	var err error
	if f.calls < len(f.errs) {
		err = f.errs[f.calls]
	}
	f.calls++
	if err != nil {
		return "", err
	}
	return "ok", nil
}

func (f *fakeAdapter) State(ctx context.Context, w Worker) (WorkerState, error) {
	return f.state, nil
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// shortTempDir keeps unix socket paths under macOS's ~104 byte limit, which
// t.TempDir paths can exceed.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "fm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func newTestService(t *testing.T, ad Adapter) (*Service, *testClock) {
	t.Helper()
	dir := t.TempDir()
	r := FileRoster{Dir: dir}
	for _, w := range []Worker{
		{Name: "coord", Harness: HarnessClaude, Coordinator: true},
		{Name: "pi-1", Harness: HarnessPi},
		{Name: "codex-1", Harness: HarnessCodex},
	} {
		if err := r.Put(w); err != nil {
			t.Fatal(err)
		}
	}
	c := &testClock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	s := &Service{
		Box:      Mailbox{Dir: dir},
		Roster:   r,
		Adapters: map[Harness]Adapter{HarnessClaude: ad, HarnessPi: ad, HarnessCodex: ad},
		Policy:   DefaultPolicy(),
		Now:      c.now,
	}
	return s, c
}
