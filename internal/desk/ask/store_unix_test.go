//go:build unix

// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package ask

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubNotifier records every notify and fails when err is set.
type stubNotifier struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (s *stubNotifier) Notify(_ context.Context, addr, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, addr+" "+text)
	return s.err
}

func newTestStore(t *testing.T, n Notifier) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, WithClock(func() time.Time { return t0 }), WithNotifier(n))
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestStoreAddRespondNotify(t *testing.T) {
	n := &stubNotifier{}
	s, dir := newTestStore(t, n)
	a, err := s.Add(Ask{Title: "Approve", Bucket: BucketNow, Notify: "t1k"})
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(a.ID) || !strings.HasPrefix(a.ID, "ask-") || !a.At.Equal(t0) {
		t.Fatalf("ask = %+v", a)
	}
	it, r, nerr, err := s.Respond(t.Context(), Response{Ask: a.ID, Kind: KindDone, Via: "test"})
	if err != nil || nerr != nil {
		t.Fatalf("respond: %v %v", err, nerr)
	}
	if it.State != StateAnswered || r.Kind != KindDone || len(it.Notified) != 1 || !it.Notified[0].OK {
		t.Fatalf("item = %+v", it)
	}
	if len(n.calls) != 1 || !strings.HasPrefix(n.calls[0], "t1k DESK-RESPONSE "+a.ID+" done") {
		t.Fatalf("notify calls = %q", n.calls)
	}
	// Answered: a second response is refused and nothing more is appended.
	before := logSize(t, s)
	if _, _, _, err := s.Respond(t.Context(), Response{Ask: a.ID, Kind: KindDone}); !errors.Is(err, ErrClosed) {
		t.Fatalf("second respond err = %v, want ErrClosed", err)
	}
	if logSize(t, s) != before {
		t.Fatal("a refused response was appended")
	}
	for _, p := range []string{filepath.Join(dir, SubDir), s.LogPath()} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s mode %v is not private", p, fi.Mode().Perm())
		}
	}
}

func logSize(t *testing.T, s *Store) int64 {
	t.Helper()
	fi, err := os.Stat(s.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func TestStoreNotifyFailureIsRecordedNotFatal(t *testing.T) {
	n := &stubNotifier{err: errors.New("hc: terminal t1k not found")}
	s, _ := newTestStore(t, n)
	a, err := s.Add(Ask{Title: "Approve", Bucket: BucketNow, Notify: "t1k"})
	if err != nil {
		t.Fatal(err)
	}
	it, _, nerr, err := s.Respond(t.Context(), Response{Ask: a.ID, Kind: KindReply, Text: "ok"})
	if err != nil {
		t.Fatalf("respond err = %v", err)
	}
	if nerr == nil || it.State != StateAnswered {
		t.Fatalf("nerr = %v, state = %v", nerr, it.State)
	}
	if len(it.Notified) != 1 || it.Notified[0].OK || !strings.Contains(it.Notified[0].Error, "not found") {
		t.Fatalf("notified = %+v", it.Notified)
	}
}

func TestStoreNoNotifyAddressSendsNothing(t *testing.T) {
	n := &stubNotifier{}
	s, _ := newTestStore(t, n)
	a, _ := s.Add(Ask{Title: "x", Bucket: BucketSoon})
	if _, _, _, err := s.Respond(t.Context(), Response{Ask: a.ID, Kind: KindDone}); err != nil {
		t.Fatal(err)
	}
	if len(n.calls) != 0 {
		t.Fatalf("notified with no address: %q", n.calls)
	}
}

// TestStoreConcurrentAnswersOnlyOneWins pins the lock: the open check and
// the append are one critical section, so racing answers cannot both land.
func TestStoreConcurrentAnswersOnlyOneWins(t *testing.T) {
	s, _ := newTestStore(t, nil)
	a, err := s.Add(Ask{Title: "race", Bucket: BucketNow})
	if err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range n {
		wg.Go(func() {
			// A separate Store per goroutine, as separate processes would have.
			other, err := Open(filepath.Dir(filepath.Dir(s.LogPath())))
			if err != nil {
				t.Error(err)
				return
			}
			if _, _, _, err := other.Respond(context.Background(), Response{Ask: a.ID, Kind: KindDone}); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, ErrClosed) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d answers landed, want 1", wins)
	}
	recs, err := s.Records()
	if err != nil {
		t.Fatal(err)
	}
	responses := 0
	for _, r := range recs {
		if r.Response != nil {
			responses++
		}
	}
	if responses != 1 {
		t.Fatalf("%d response records, want 1", responses)
	}
}

func TestStoreReadFromSkipsPartialAndBadLines(t *testing.T) {
	s, _ := newTestStore(t, nil)
	a, _ := s.Add(Ask{Title: "one", Bucket: BucketNow})
	f, err := os.OpenFile(s.LogPath(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("not json\n{\"v\":1}\n{\"v\":1,\"ask\":{\"id\":\"ask-0000")
	_ = f.Close()
	recs, next, bad, err := s.ReadFrom(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Ask.ID != a.ID || bad != 2 {
		t.Fatalf("recs=%d bad=%d", len(recs), bad)
	}
	if next >= logSize(t, s) {
		t.Fatalf("next %d consumed the partial line (size %d)", next, logSize(t, s))
	}
	more, next2, _, err := s.ReadFrom(next)
	if err != nil || len(more) != 0 || next2 != next {
		t.Fatalf("re-read from next: %d recs, next %d, %v", len(more), next2, err)
	}
	if _, _, _, err := s.ReadFrom(1 << 40); err == nil {
		t.Fatal("an offset past the end was accepted")
	}
}

func TestStoreRefusesSymlinkedLogAndDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, SubDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, SubDir, LogName)); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Ask{Title: "x", Bucket: BucketNow}); err == nil {
		t.Fatal("appended through a symlinked log")
	}
	if data, _ := os.ReadFile(target); len(data) != 0 { //nolint:gosec // G304: a path this test created
		t.Fatal("the symlink target was written")
	}

	dir2 := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(dir2, SubDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir2); err == nil {
		t.Fatal("opened a symlinked asks directory")
	}
}

func TestOpenReadOnlyCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "desk")
	s, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.Items()
	if err != nil || len(items) != 0 {
		t.Fatalf("items = %v, %v", items, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a read-only open created the desk directory")
	}
	if _, err := s.Add(Ask{Title: "x", Bucket: BucketNow}); err == nil {
		t.Fatal("a read-only store accepted a write")
	}
}

func TestOpenNarrowsABroadDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, SubDir), 0o755); err != nil { //nolint:gosec // G301: the test needs a broad directory to prove Open narrows it
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, SubDir), 0o755); err != nil { //nolint:gosec // G302: the test needs a broad directory to prove Open narrows it
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(dir, SubDir))
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}

// TestStoreAppendHealsATornLastLine pins the append's repair: a writer that
// died mid-line must not take the next record down with it.
func TestStoreAppendHealsATornLastLine(t *testing.T) {
	s, _ := newTestStore(t, nil)
	a, err := s.Add(Ask{Title: "one", Bucket: BucketNow})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.LogPath(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"v":1,"response":{"id":"resp-dead`)
	_ = f.Close()
	it, _, _, err := s.Respond(t.Context(), Response{Ask: a.ID, Kind: KindDone})
	if err != nil {
		t.Fatal(err)
	}
	if it.State != StateAnswered {
		t.Fatalf("the answer was lost behind the torn line: state %s", it.State)
	}
	_, _, bad, err := s.ReadFrom(0)
	if err != nil || bad != 1 {
		t.Fatalf("bad = %d (want the torn line alone), err %v", bad, err)
	}
}

func TestStoreRecordsOrigin(t *testing.T) {
	s, _ := newTestStore(t, nil)
	for via, want := range map[string]string{"web": OriginOperator, "ui": OriginOperator, "cli": OriginAgent, "": OriginAgent} {
		a, err := s.Add(Ask{Title: "o " + via, Bucket: BucketNow, From: "origin-" + via})
		if err != nil {
			t.Fatal(err)
		}
		// A caller cannot claim an origin; the store sets it from via.
		_, r, _, err := s.Respond(t.Context(), Response{Ask: a.ID, Kind: KindDone, Via: via, Origin: OriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if r.Origin != want {
			t.Errorf("via %q: origin %q, want %q", via, r.Origin, want)
		}
	}
}

func TestStoreCapsOpenAsksPerAsker(t *testing.T) {
	s, _ := newTestStore(t, nil)
	var first Ask
	for i := range MaxOpenPerAsker {
		a, err := s.Add(Ask{Title: "a", Bucket: BucketSoon, From: "busy"})
		if err != nil {
			t.Fatalf("ask %d: %v", i, err)
		}
		if i == 0 {
			first = a
		}
	}
	if _, err := s.Add(Ask{Title: "one too many", Bucket: BucketSoon, From: "busy"}); !errors.Is(err, ErrTooMany) || !strings.Contains(err.Error(), "busy already has 20 open asks") {
		t.Fatalf("over the cap: %v", err)
	}
	if _, err := s.Add(Ask{Title: "someone else", Bucket: BucketSoon, From: "calm"}); err != nil {
		t.Fatalf("another asker was capped: %v", err)
	}
	if _, _, _, err := s.Respond(t.Context(), Response{Ask: first.ID, Kind: KindDone, Via: "ui"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Ask{Title: "room again", Bucket: BucketSoon, From: "busy"}); err != nil {
		t.Fatalf("an answer did not free a slot: %v", err)
	}
}

// TestStoreCapsOpenAsksOverall: varying --from does not get past the
// desk-wide limit.
func TestStoreCapsOpenAsksOverall(t *testing.T) {
	s, _ := newTestStore(t, nil)
	for i := range MaxOpenAsks {
		if _, err := s.Add(Ask{Title: "a", Bucket: BucketSoon, From: fmt.Sprintf("asker-%d", i)}); err != nil {
			t.Fatalf("ask %d: %v", i, err)
		}
	}
	_, err := s.Add(Ask{Title: "one more", Bucket: BucketSoon, From: "fresh-name"})
	if !errors.Is(err, ErrTooMany) || !strings.Contains(err.Error(), "200 open asks (the limit for all askers)") {
		t.Fatalf("over the desk-wide cap: %v", err)
	}
}

// TestStoreRefusesAsksWhenTheLogIsFull: past the ask write cap, new asks are
// refused with a clear error while answers still land.
func TestStoreRefusesAsksWhenTheLogIsFull(t *testing.T) {
	s, _ := newTestStore(t, nil)
	a, err := s.Add(Ask{Title: "keep me answerable", Bucket: BucketNow})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.LogPath(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Filler lines: not records (skipped on read) but real bytes.
	pad := append(bytes.Repeat([]byte("x"), 1<<20-1), '\n')
	for f2 := 0; f2 < maxAskLogBytes>>20; f2++ {
		if _, err := f.Write(pad); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	if _, err := s.Add(Ask{Title: "one too many", Bucket: BucketNow}); !errors.Is(err, ErrLogFull) || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("Add on a full log: %v", err)
	}
	if _, _, _, err := s.Respond(t.Context(), Response{Ask: a.ID, Kind: KindDone, Via: "ui"}); err != nil {
		t.Fatalf("an answer was refused on a log full for asks: %v", err)
	}
}
