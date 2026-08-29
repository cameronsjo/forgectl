package docs

import (
	"image"
	"image/color"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi/kitty"
)

func TestParseGraphicsMode(t *testing.T) {
	for _, value := range []string{"auto", "kitty", "off"} {
		if got, err := ParseGraphicsMode(value); err != nil || string(got) != value {
			t.Fatalf("ParseGraphicsMode(%q) = %q, %v", value, got, err)
		}
	}
	if _, err := ParseGraphicsMode("sixel"); err == nil {
		t.Fatal("ParseGraphicsMode(sixel) succeeded")
	}
}

func TestKittyGraphicsEnabled(t *testing.T) {
	env := map[string]string{"TERM": "xterm-256color", "TERM_PROGRAM": "ghostty"}
	getenv := func(key string) string { return env[key] }
	if !KittyGraphicsEnabled(GraphicsAuto, getenv) {
		t.Fatal("auto did not detect Ghostty")
	}
	if KittyGraphicsEnabled(GraphicsOff, getenv) {
		t.Fatal("off enabled graphics")
	}
	if !KittyGraphicsEnabled(GraphicsKitty, func(string) string { return "" }) {
		t.Fatal("forced kitty did not enable graphics")
	}
}

func TestKittyImageBlock_UsesPlaceholdersAndContentIdentity(t *testing.T) {
	red := image.NewRGBA(image.Rect(0, 0, 4, 2))
	blue := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			red.Set(x, y, color.RGBA{R: 255, A: 255})
			blue.Set(x, y, color.RGBA{B: 255, A: 255})
		}
	}
	transmission, placeholders, redID, err := KittyImageBlock(red, 8)
	if err != nil {
		t.Fatal(err)
	}
	_, _, blueID, err := KittyImageBlock(blue, 8)
	if err != nil {
		t.Fatal(err)
	}
	if redID == blueID {
		t.Fatal("same-sized images received the same ID")
	}
	if !strings.Contains(transmission, "\x1b_G") || !strings.Contains(transmission, ";") || strings.ContainsRune(transmission, kitty.Placeholder) {
		t.Fatalf("transmission = %q", transmission)
	}
	if !strings.ContainsRune(placeholders, kitty.Placeholder) || strings.Contains(placeholders, "\x1b_G") {
		t.Fatalf("placeholders = %q", placeholders)
	}
	if !utf8.ValidString(transmission) || !utf8.ValidString(placeholders) {
		t.Fatal("Kitty output is not valid UTF-8")
	}
	cleanup := KittyCleanupSequence([]uint32{redID})
	if !strings.Contains(cleanup, "a=d") || !strings.Contains(cleanup, "d=I") {
		t.Fatalf("cleanup = %q", cleanup)
	}
}
