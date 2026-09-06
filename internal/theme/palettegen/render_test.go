package palettegen

import (
	"go/format"
	"strings"
	"testing"
)

// fixturePalette carries exactly the sixteen distinct _palette.json keys
// theme.ArtificerRoleSources needs (attention feeds two roles), for both
// "dark" and "light" — a small, hand-written stand-in for the real vendored
// file so this test does not depend on it.
const fixturePalette = `{
  "$version": "9.9.9",
  "dark": {
    "accent": "#111111",
    "success": "#222222",
    "urgentText": "#333333",
    "attention": "#444444",
    "fgMuted": "#555555",
    "fgSecondary": "#666666",
    "fgDisabled": "#777777",
    "fg": "#888888",
    "steel": "#999999",
    "brandPurpleBright": "#aaaaaa",
    "bgRaised": "#bbbbbb",
    "accentFill": "#cccccc",
    "ink": "#dddddd",
    "urgent": "#eeeeee",
    "ivory": "#ffffff",
    "bg": "#000000"
  },
  "light": {
    "accent": "#211111",
    "success": "#222212",
    "urgentText": "#333313",
    "attention": "#444414",
    "fgMuted": "#555515",
    "fgSecondary": "#666616",
    "fgDisabled": "#777717",
    "fg": "#888818",
    "steel": "#999919",
    "brandPurpleBright": "#aaaa1a",
    "bgRaised": "#bbbb1b",
    "accentFill": "#cccc1c",
    "ink": "#dddd1d",
    "urgent": "#eeee1e",
    "ivory": "#ffff1f",
    "bg": "#00001f"
  }
}`

func TestRender_ProducesValidGo(t *testing.T) {
	out, err := Render([]byte(fixturePalette))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, err := format.Source(out); err != nil {
		t.Fatalf("Render produced source go/format rejects: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `func Artificer() Palette`) {
		t.Errorf("Render output does not declare Artificer():\n%s", out)
	}
	if !strings.Contains(string(out), `Dark: "#111111"`) || !strings.Contains(string(out), `Light: "#211111"`) {
		t.Errorf("Render output missing the resolved accent hexes:\n%s", out)
	}
	if !strings.Contains(string(out), "version 9.9.9") {
		t.Errorf("Render output does not name the palette's $version:\n%s", out)
	}
}

func TestRender_TwoRolesShareTheAttentionKey(t *testing.T) {
	out, err := Render([]byte(fixturePalette))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	for _, role := range []string{"RoleWarn", "RoleActive"} {
		idx := strings.Index(s, role+":")
		if idx == -1 {
			t.Fatalf("Render output missing %s:\n%s", role, s)
		}
		line := s[idx : idx+strings.Index(s[idx:], "\n")]
		if !strings.Contains(line, `"#444444"`) || !strings.Contains(line, `"#444414"`) {
			t.Errorf("%s line %q does not carry the shared attention hexes", role, line)
		}
	}
}

func TestRender_MissingKeyErrors(t *testing.T) {
	broken := strings.Replace(fixturePalette, `"accent": "#111111",`, "", 1)
	_, err := Render([]byte(broken))
	if err == nil {
		t.Fatal("Render: got nil error for a palette missing a required key, want an error")
	}
	if !strings.Contains(err.Error(), "accent") {
		t.Errorf("Render error %q does not name the missing key", err.Error())
	}
}

func TestRender_MissingLightKeyErrors(t *testing.T) {
	broken := strings.Replace(fixturePalette, `"ivory": "#ffff1f",`, "", 1)
	_, err := Render([]byte(broken))
	if err == nil {
		t.Fatal("Render: got nil error for a palette missing a required light key, want an error")
	}
	if !strings.Contains(err.Error(), "light.ivory") {
		t.Errorf("Render error %q does not name the missing light key", err.Error())
	}
}

func TestRender_MalformedJSONErrors(t *testing.T) {
	if _, err := Render([]byte("{not json")); err == nil {
		t.Fatal("Render: got nil error for malformed JSON, want an error")
	}
}

func TestRender_MissingVersionErrors(t *testing.T) {
	noVersion := strings.Replace(fixturePalette, `"$version": "9.9.9",`, "", 1)
	if _, err := Render([]byte(noVersion)); err == nil {
		t.Fatal("Render: got nil error for a palette with no $version, want an error")
	}
}
