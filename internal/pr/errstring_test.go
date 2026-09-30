package pr

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// panicError stands in for Go 1.26's os.errSymlink, whose Error method is a
// panic and which os.Root.RemoveAll can leak wrapped in a *fs.PathError
// (forgectl#764).
type panicError struct{}

func (panicError) Error() string { panic("panicError is not user-visible") }

// Mutations that turn it red: return err.Error() from safeErrString with no
// recover (the panicking cases crash the test); drop the *fs.PathError
// prefix in unrenderableErrText (the wrapped case loses its Op and Path).
func TestSafeErrString(t *testing.T) {
	wrapped := &fs.PathError{Op: "RemoveAll", Path: "sub", Err: panicError{}}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"plain", errors.New("boom"), "boom"},
		{"bare panic", panicError{}, errTextUnavailable},
		{"path error", wrapped, "RemoveAll sub: " + errTextUnavailable},
		{"panicking wrapper", panicWrapper{wrapped}, "RemoveAll sub: " + errTextUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeErrString(tc.err); got != tc.want {
				t.Errorf("safeErrString = %q, want %q", got, tc.want)
			}
		})
	}
}

// panicWrapper panics in its own Error but unwraps to a *fs.PathError, so
// the categorical text must come from errors.As, not from the message.
type panicWrapper struct{ inner error }

func (panicWrapper) Error() string   { panic("panicWrapper is not user-visible") }
func (w panicWrapper) Unwrap() error { return w.inner }

// renderableErr keeps a renderable error as it is (so errors.Is still
// matches) and replaces a panicking one.
//
// Mutation that turns it red: return err unconditionally from renderableErr
// (the panicking case's Error() crashes the test).
func TestRenderableErr(t *testing.T) {
	if renderableErr(nil) != nil {
		t.Error("renderableErr(nil) != nil")
	}
	plain := &fs.PathError{Op: "unlinkat", Path: "x", Err: fs.ErrPermission}
	if got := renderableErr(plain); got != error(plain) {
		t.Errorf("renderableErr(renderable) = %v, want it unchanged", got)
	}
	got := renderableErr(&fs.PathError{Op: "RemoveAll", Path: "sub", Err: panicError{}})
	if msg := got.Error(); msg != "RemoveAll sub: "+errTextUnavailable {
		t.Errorf("renderableErr(panicking).Error() = %q", msg)
	}
}

// A removal whose error panics when rendered, as Go 1.26's leaked errSymlink
// does, completes its audit row as failed with the categorical text and
// returns an error that renders, instead of crashing mid-hold and leaving the
// intent row dangling (forgectl#764).
//
// Mutations that turn it red: render the cause with cause.Error() in
// completeRepairRow (the test panics); drop renderableErr around
// findingsRemoveAll in removeFindingsDirAudited (the returned error's text
// carries the fmt PANIC marker instead of the categorical text).
func TestFindingsRemove_PanickingRemovalErrorCompletesRow(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	target := filepath.Join(store, findingsDirPrefix+"panics")
	mustMkdir(t, target)

	orig := findingsRemoveAll
	t.Cleanup(func() { findingsRemoveAll = orig })
	findingsRemoveAll = func(*os.Root, *os.Root, string, fs.FileInfo) error {
		return &fs.PathError{Op: "RemoveAll", Path: "sub", Err: panicError{}}
	}

	removed, err := c.FindingsRemove(t.Context(), []string{target})
	if err == nil {
		t.Fatal("FindingsRemove succeeded, want the injected failure")
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want none", removed)
	}
	if msg := err.Error(); !strings.Contains(msg, errTextUnavailable) || strings.Contains(msg, "PANIC") {
		t.Errorf("returned error = %q, want the categorical text and no panic marker", msg)
	}
	rows := auditRows(t, c)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want the intent and its completion: %+v", len(rows), rows)
	}
	if rows[1].Outcome != repairOutcomeFailed || rows[1].Error != "RemoveAll sub: "+errTextUnavailable {
		t.Errorf("completion = %q / %q, want failed with the categorical text", rows[1].Outcome, rows[1].Error)
	}
}

// completeRepairRow renders its cause through safeErrString itself, so a
// caller that hands it a panicking error unsanitized still completes the row
// rather than crashing between intent and completion.
//
// Mutation that turns it red: render the cause with cause.Error() in
// completeRepairRow (the test panics).
func TestCompleteRepairRow_PanickingCauseIsCategorical(t *testing.T) {
	c := findingsClient(t, t.TempDir())
	row := RepairRow{Verb: auditVerbFindingsCleanup, RecordPath: "/x"}
	err := c.withLifecycleLock(t.Context(), auditVerbFindingsCleanup, func() error {
		id, err := c.beginRepairRow(row)
		if err != nil {
			return err
		}
		c.completeRepairRow(id, row, &fs.PathError{Op: "RemoveAll", Path: "sub", Err: panicError{}})
		return nil
	})
	if err != nil {
		t.Fatalf("lock hold: %v", err)
	}
	rows := auditRows(t, c)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want the intent and its completion: %+v", len(rows), rows)
	}
	if rows[1].Outcome != repairOutcomeFailed || rows[1].Error != "RemoveAll sub: "+errTextUnavailable {
		t.Errorf("completion = %q / %q, want failed with the categorical text", rows[1].Outcome, rows[1].Error)
	}
}
