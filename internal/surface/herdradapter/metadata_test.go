//go:build unix

package herdradapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func paneGetTerminalJSON(pane, ws, terminal string) []byte {
	return []byte(`{"id":"cli:pane:get","result":{"pane":{"pane_id":"` + pane + `","workspace_id":"` + ws +
		`","terminal_id":"` + terminal + `","agent":"claude","agent_status":"idle"}}}`)
}

// TestShowWorkerName sets the worker token on the owned workspace only.
func TestShowWorkerName(t *testing.T) {
	ctx := context.Background()

	t.Run("reports the name on the owned workspace", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		if err := a.ShowWorkerName(ctx, ref, "fix-login"); err != nil {
			t.Fatalf("ShowWorkerName: %v", err)
		}
		cmd, ok := commandOfKind(run.calls(), exec.KindHerdrMetadata)
		if !ok {
			t.Fatal("no metadata call was made")
		}
		assertArgv(t, cmd, []exec.Arg{exec.MustFixed("--session"), exec.Opaque(a.Session()),
			exec.MustFixed("workspace"), exec.MustFixed("report-metadata"), exec.Opaque(wsA),
			exec.MustFixed("--source"), exec.MustFixed(MetadataSource),
			exec.MustFixed("--token"), exec.Opaque("worker=fix-login")})
	})

	t.Run("a workspace without our marker is never written", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, "someone-else"}))
		if err := a.ShowWorkerName(ctx, ref, "fix-login"); !errors.Is(err, ErrScreenUnreadable) {
			t.Fatalf("err = %v, want ErrScreenUnreadable", err)
		}
		if _, ok := commandOfKind(run.calls(), exec.KindHerdrMetadata); ok {
			t.Error("a workspace we do not own was written")
		}
	})

	t.Run("a herdr refusal is ErrMetadataFailed", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.on(exec.KindHerdrMetadata, func() (exec.SensitiveResult, error) {
			return exec.SensitiveResult{}, exec.SensitiveErrorForTest(exec.KindHerdrMetadata, exec.OutcomeExit)
		})
		if err := a.ShowWorkerName(ctx, ref, "fix-login"); !errors.Is(err, ErrMetadataFailed) {
			t.Fatalf("err = %v, want ErrMetadataFailed", err)
		}
	})
}

// TestTagPane writes sorted tokens to the owned root pane, and refuses a
// token herdr would refuse or alter before any herdr call.
func TestTagPane(t *testing.T) {
	ctx := context.Background()

	t.Run("sets and clears tokens on the owned root pane", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.reply1(exec.KindHerdrPaneStatus, paneGetJSON(paneA, wsA, "claude", "idle"))
		pane, err := a.TagPane(ctx, ref, map[string]string{"task": "PAP-12", "issue": "forgectl#1239"}, []string{"run"})
		if err != nil {
			t.Fatalf("TagPane: %v", err)
		}
		if pane != paneA {
			t.Errorf("pane = %q, want %q", pane, paneA)
		}
		cmd, ok := commandOfKind(run.calls(), exec.KindHerdrMetadata)
		if !ok {
			t.Fatal("no metadata call was made")
		}
		assertArgv(t, cmd, []exec.Arg{exec.MustFixed("--session"), exec.Opaque(a.Session()),
			exec.MustFixed("pane"), exec.MustFixed("report-metadata"), exec.Opaque(paneA),
			exec.MustFixed("--source"), exec.MustFixed(MetadataSource),
			exec.MustFixed("--token"), exec.Opaque("issue=forgectl#1239"),
			exec.MustFixed("--token"), exec.Opaque("task=PAP-12"),
			exec.MustFixed("--clear-token"), exec.Opaque("run")})
	})

	refused := []struct {
		name  string
		set   map[string]string
		clear []string
	}{
		{"nothing", nil, nil},
		{"a dotted key", map[string]string{"a.b": "x"}, nil},
		{"a long key", map[string]string{strings.Repeat("k", 33): "x"}, nil},
		{"an empty value", map[string]string{"k": " "}, nil},
		{"a padded value", map[string]string{"k": " x"}, nil},
		{"a control character", map[string]string{"k": "a\x1b[31mb"}, nil},
		{"a bidi character", map[string]string{"k": "a\u202eb"}, nil},
		{"a long value", map[string]string{"k": strings.Repeat("v", 81)}, nil},
		{"a key both set and cleared", map[string]string{"k": "v"}, []string{"k"}},
		{"a key cleared twice", nil, []string{"k", "k"}},
		{"a bad cleared key", nil, []string{"k=v"}},
		{"a dash-leading key", map[string]string{"-x": "1"}, nil},
		{"a dash-leading cleared key", nil, []string{"-x"}},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			a, run, ref := startClean(t)
			before := len(run.calls())
			if _, err := a.TagPane(ctx, ref, tc.set, tc.clear); !errors.Is(err, ErrBadToken) {
				t.Fatalf("err = %v, want ErrBadToken", err)
			}
			if len(run.calls()) != before {
				t.Error("herdr was called for a refused tag")
			}
		})
	}

	t.Run("refuses more than 16 tokens", func(t *testing.T) {
		a, _, ref := startClean(t)
		set := map[string]string{}
		for i := range MaxTokensPerReport + 1 {
			set["k"+strings.Repeat("x", i)] = "v"
		}
		if _, err := a.TagPane(ctx, ref, set, nil); !errors.Is(err, ErrBadToken) {
			t.Fatalf("err = %v, want ErrBadToken", err)
		}
	})

	t.Run("a pane herdr places elsewhere is never written", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.reply1(exec.KindHerdrPaneStatus, paneGetJSON("w9:p1", "w9", "claude", "idle"))
		if _, err := a.TagPane(ctx, ref, map[string]string{"k": "v"}, nil); !errors.Is(err, ErrScreenUnreadable) {
			t.Fatalf("err = %v, want ErrScreenUnreadable", err)
		}
		if _, ok := commandOfKind(run.calls(), exec.KindHerdrMetadata); ok {
			t.Error("a pane herdr placed elsewhere was written")
		}
	})
}

// TestTerminalID reads the terminal_id of the referenced pane and refuses an
// answer about another pane.
func TestTerminalID(t *testing.T) {
	ctx := context.Background()

	t.Run("reads the root pane's terminal_id", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrPaneStatus, paneGetTerminalJSON(paneA, wsA, "term-7"))
		got, err := a.TerminalID(ctx, ref)
		if err != nil {
			t.Fatalf("TerminalID: %v", err)
		}
		if got != "term-7" {
			t.Errorf("terminal_id = %q, want term-7", got)
		}
	})

	t.Run("an answer for another pane is unreadable", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrPaneStatus, paneGetTerminalJSON("w9:p1", "w9", "term-7"))
		if _, err := a.TerminalID(ctx, ref); !errors.Is(err, ErrScreenUnreadable) {
			t.Fatalf("err = %v, want ErrScreenUnreadable", err)
		}
	})

	t.Run("a reply with no terminal_id is unreadable", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrPaneStatus, paneGetJSON(paneA, wsA, "claude", "idle"))
		if _, err := a.TerminalID(ctx, ref); !errors.Is(err, ErrScreenUnreadable) {
			t.Fatalf("err = %v, want ErrScreenUnreadable", err)
		}
	})
}
