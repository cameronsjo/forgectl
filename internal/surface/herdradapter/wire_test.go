package herdradapter

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr/wire"
)

// The adapter reads herdr's wire format through internal/herdr/wire, as the
// client does (#722). These tests run the adapter's parsers against the same
// captured fixtures the client's tests use, so a changed envelope, captured
// into internal/herdr/testdata, fails here and in the client's tests at once.

// sharedFixture reads one of the captured herdr replies in internal/herdr/testdata.
func sharedFixture(t *testing.T, name string) exec.BoundedOutput {
	t.Helper()
	root, err := os.OpenRoot("../../herdr/testdata")
	if err != nil {
		t.Fatalf("open shared testdata: %v", err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Errorf("close shared testdata: %v", err)
		}
	}()
	b, err := root.ReadFile(name)
	if err != nil {
		t.Fatalf("read shared fixture %s: %v", name, err)
	}
	return exec.BoundedOutputForTest(b, exec.OutputComplete)
}

func TestParsersReadTheCapturedEnvelopes(t *testing.T) {
	rows, err := parseWorkspaceList(sharedFixture(t, "workspace_list.json"))
	if err != nil {
		t.Fatalf("parseWorkspaceList(captured listing): %v", err)
	}
	if len(rows) == 0 {
		t.Error("the captured listing parsed to no rows")
	}
	if got := errorCode(sharedFixture(t, "err_workspace_not_found.json")); got != "workspace_not_found" {
		t.Errorf("errorCode(captured refusal) = %q, want workspace_not_found", got)
	}
}

// The renamed-result row is held by two layers, wire.ErrNoResult and the
// listing's nil-member check; it goes red (reading as an empty listing, which
// locate and reconcile take as absence) only when both are removed. Each layer
// alone is pinned by TestDecodeResult and
// TestWorkspaceListingWithoutItsListIsNotEmpty. errorCode returns "" for a
// code-less envelope whether or not DecodeError accepts it, so the refusal row
// pins the reader, and the captured fixtures above pin the envelope itself.
func TestChangedEnvelopeFixturesFailClosed(t *testing.T) {
	if rows, err := parseWorkspaceList(sharedFixture(t, "changed_result_envelope.json")); err == nil {
		t.Errorf("a renamed result envelope read as a listing of %d rows", len(rows))
	}
	if got := errorCode(sharedFixture(t, "changed_error_envelope.json")); got != "" {
		t.Errorf("errorCode(code-less envelope) = %q, want none", got)
	}
}

// Mutation: make the `Workspaces == nil` case in parseWorkspaceList return an
// empty map and the missing-member rows go red.
func TestWorkspaceListingWithoutItsListIsNotEmpty(t *testing.T) {
	for name, raw := range map[string]string{
		"result missing":     `{"id":"cli:workspace:list"}`,
		"result null":        `{"id":"cli:workspace:list","result":null}`,
		"workspaces missing": `{"id":"cli:workspace:list","result":{"type":"workspace_list"}}`,
		"workspaces null":    `{"id":"cli:workspace:list","result":{"workspaces":null}}`,
		"workspaces renamed": `{"id":"cli:workspace:list","result":{"items":[]}}`,
	} {
		out := exec.BoundedOutputForTest([]byte(raw), exec.OutputComplete)
		if rows, err := parseWorkspaceList(out); err == nil {
			t.Errorf("%s: read as a listing of %d rows", name, len(rows))
		}
	}
	empty := exec.BoundedOutputForTest([]byte(`{"id":"x","result":{"workspaces":[]}}`), exec.OutputComplete)
	if rows, err := parseWorkspaceList(empty); err != nil || len(rows) != 0 {
		t.Errorf("an empty listing = %v, %v; want zero rows", rows, err)
	}
}

// Mutation: drop the wire.CheckOperand call from validSessionName and the
// leading-dash row goes red (the charset admits '-'); the over-length row
// pins the shared limit.
func TestSessionNameMeetsTheSharedOperandCheck(t *testing.T) {
	for name, s := range map[string]string{
		"leading dash": "-fleet",
		"over length":  strings.Repeat("f", wire.MaxOperandLen+1),
	} {
		if err := validSessionName(s); !errors.Is(err, ErrResolveSession) {
			t.Errorf("%s: validSessionName = %v, want ErrResolveSession", name, err)
		}
	}
	if err := validSessionName(strings.Repeat("f", wire.MaxOperandLen)); err != nil {
		t.Errorf("a name at the limit was refused: %v", err)
	}
}
