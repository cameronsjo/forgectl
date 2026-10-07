// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// drive runs cmd and feeds every message it produces back into the model,
// as the program would, until no command is left. Ticks are not followed.
func driveWatch(t *testing.T, w runWatchModel, cmd tea.Cmd) runWatchModel {
	t.Helper()
	for i := 0; cmd != nil && i < 20; i++ {
		msg := cmd()
		if _, tick := msg.(deskTickMsg); tick {
			return w
		}
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				w = driveWatch(t, w, c)
			}
			return w
		}
		next, c := w.Update(msg)
		w = next.(runWatchModel) //nolint:forcetypeassert // test
		cmd = c
	}
	return w
}

func lensLogSource(t *testing.T, body string) runview.Source {
	t.Helper()
	l, err := runview.ParseLens("t", []byte("format='text'\n[[rule]]\naction='start'\nmatch='^go (?P<step>\\w+)'\n[[rule]]\naction='end'\nmatch='^done'\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := runview.NewLensSource(p, l)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestRunWatch_DrawsALensRunAndQQuits(t *testing.T) {
	src := lensLogSource(t, "go fetch\nnoise\ngo build\n")
	w := newRunWatchModel(t.Context(), src, RunWatchOptions{Poll: time.Millisecond, Theme: theme.Default(), ASCII: true})
	next, _ := w.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	w = driveWatch(t, next.(runWatchModel), w.Init()) //nolint:forcetypeassert // test
	view := ansi.Strip(w.View().Content)
	for _, want := range []string{"log app.log", "fetch", "build", "3 events", "> start"} {
		if !strings.Contains(view, want) {
			t.Errorf("the live view is missing %q:\n%s", want, view)
		}
	}
	if !w.m.rv.polls() {
		t.Error("a lens run with no end yet should keep polling")
	}
	next, cmd := w.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("q should quit the live view")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok || next.(runWatchModel).missing { //nolint:forcetypeassert // test
		t.Error("q should quit, and not as a missing run")
	}
}

// An ended log reads ended but is still followed: the app may start again
// and keep appending to the same file.
func TestRunWatch_EndedLensRunReadsEndedAndKeepsFollowing(t *testing.T) {
	src := lensLogSource(t, "go fetch\ndone\n")
	w := newRunWatchModel(t.Context(), src, RunWatchOptions{Poll: time.Millisecond, Theme: theme.Default(), ASCII: true})
	w = driveWatch(t, w, w.Init())
	if !w.m.rv.polls() {
		t.Error("a log should be followed even after its run ended")
	}
	if view := ansi.Strip(w.View().Content); !strings.Contains(view, "ended") {
		t.Errorf("an ended lens run should read ended:\n%s", view)
	}
}

func TestRunWatch_MissingRunQuitsAsMissing(t *testing.T) {
	src := lensLogSource(t, "go fetch\n")
	w := newRunWatchModel(t.Context(), src, RunWatchOptions{Poll: time.Millisecond, Theme: theme.Default(), Name: "other.log"})
	w = driveWatch(t, w, w.Init())
	if !w.missing {
		t.Error("a run that is not there should quit as missing")
	}
}
