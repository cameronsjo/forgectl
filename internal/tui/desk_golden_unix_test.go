// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/theme"
)

// deskGoldenSizes are the window sizes the dashboard's layouts are pinned at:
// single pane, the mid layout in two heights, the wide layout, and the floor.
var deskGoldenSizes = [][2]int{{60, 30}, {120, 30}, {120, 50}, {160, 50}, {40, 12}}

func TestGoldenDesk(t *testing.T) {
	forceTrueColor(t)
	for _, sz := range deskGoldenSizes {
		t.Run(fmt.Sprintf("%dx%d", sz[0], sz[1]), func(t *testing.T) {
			m := goldenDesk(t, false, sz[0], sz[1])
			assertGolden(t, fmt.Sprintf("desk_%dx%d", sz[0], sz[1]), m.View().Content)
		})
	}
}

// TestGoldenDeskNoColor draws the same screens through the NO_COLOR writer
// the program's output takes, so the plain layout is pinned too.
func TestGoldenDeskNoColor(t *testing.T) {
	for _, sz := range deskGoldenSizes {
		t.Run(fmt.Sprintf("%dx%d", sz[0], sz[1]), func(t *testing.T) {
			m := goldenDesk(t, false, sz[0], sz[1])
			assertGolden(t, fmt.Sprintf("desk_%dx%d_nocolor", sz[0], sz[1]), plainDesk(t, m.View().Content))
		})
	}
}

// TestGoldenDeskNoIcons pins the ASCII marks, with colour removed so the
// golden shows which characters carry the meaning.
func TestGoldenDeskNoIcons(t *testing.T) {
	for _, sz := range [][2]int{{120, 30}, {160, 50}} {
		t.Run(fmt.Sprintf("%dx%d", sz[0], sz[1]), func(t *testing.T) {
			m := goldenDesk(t, true, sz[0], sz[1])
			out := plainDesk(t, m.View().Content)
			for _, glyph := range []string{"✓", "✗", "◐", "↺", "⟲"} {
				if strings.Contains(out, glyph) {
					t.Errorf("the no-icons render still carries %q", glyph)
				}
			}
			assertGolden(t, fmt.Sprintf("desk_%dx%d_noicons", sz[0], sz[1]), out)
		})
	}
}

// goldenDesk opens the fixture on the flow view. The dashboard opens on the
// queue when items wait, which the queue golden covers.
func goldenDesk(t *testing.T, noIcons bool, w, h int) deskModel {
	t.Helper()
	return pressDesk(t, openDesk(t, goldenDeskOptions(noIcons), w, h), key("1"))
}

// plainDesk removes colour from a rendered screen the way the program's
// writer does under NO_COLOR.
func plainDesk(t *testing.T, view string) string {
	t.Helper()
	var buf bytes.Buffer
	w := theme.Default().Writer(&buf, []string{"NO_COLOR=1", "TERM=xterm-256color"})
	if _, err := w.Write([]byte(view)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "\x1b") {
		t.Fatal("the no-color render still carries escapes")
	}
	return buf.String()
}

func TestGoldenDeskQueueAndOtherViews(t *testing.T) {
	forceTrueColor(t)
	for _, v := range []string{"2", "3", "4", "5", "6"} {
		t.Run("view"+v, func(t *testing.T) {
			m := openDesk(t, goldenDeskOptions(false), 120, 30)
			m = pressDesk(t, m, key("]")) // beta-run-7 has a summary
			m = pressDesk(t, m, key(v))
			assertGolden(t, "desk_120x30_view"+v+"_nocolor", plainDesk(t, m.View().Content))
		})
	}
}
