package docs

import (
	"strings"
	"testing"
)

// An alt containing '#' must survive; a hostile alt must not open a tag or
// break out of the attribute (bluemonday escapes what the pattern lets by,
// and the pattern refuses the markup characters outright).
func TestRender_ImgAltHash(t *testing.T) {
	got, err := Render([]byte("![#1 chart](i.png)\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `alt="#1 chart"`) {
		t.Errorf("alt with # dropped: %q", got)
	}

	for _, alt := range []string{`#x" onerror="alert(1)`, `#<script>alert(1)</script>`, "#x&#x22;y", `#a;b`} {
		got, err := Render([]byte("![" + alt + "](i.png)\n"))
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"onerror", "<script", `alt="#x"`} {
			if strings.Contains(got, bad) {
				t.Errorf("hostile alt %q leaked %q: %q", alt, bad, got)
			}
		}
	}
}
