package docs

// Reader href contract (docHref, DocURL):
//   [x] Happy: '%', a non-ASCII letter, a space, '#' and '?' in a filename are
//       percent-escaped in every emitted href
//   [x] Happy: DocURL is docHref behind the server's scheme and host
//   [x] Happy: every emitted href GETs back the doc it names (round trip)

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDocHref_EscapesAndRoundTrips(t *testing.T) {
	cases := []struct {
		name, title, wantPath string
	}{
		{"100%.md", "Percent", "100%25.md"},
		{"é.md", "Accent", "%C3%A9.md"},
		{"a b.md", "Space", "a%20b.md"},
		{"a#b.md", "Hash", "a%23b.md"},
		{"a?b.md", "Query", "a%3Fb.md"},
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "README.md"), "# Top\n")
	for _, c := range cases {
		writeFile(t, filepath.Join(dir, c.name), "# "+c.title+"\n")
	}
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	label := idx.Roots()[0].Label
	h := testHandler(idx)
	sidenav := getBody(t, h, "/doc/"+label+"/README.md")
	info := ServerInfo{Addr: "127.0.0.1:3590"}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			href := docHref(label, c.name)
			if want := "/doc/" + label + "/" + c.wantPath; href != want {
				t.Fatalf("docHref = %q, want %q", href, want)
			}
			if got, want := info.DocURL(label, c.name), "http://"+info.Addr+href; got != want {
				t.Errorf("DocURL = %q, want %q", got, want)
			}
			if !strings.Contains(sidenav, `href="`+href+`"`) {
				t.Errorf("sidenav does not emit href %q:\n%s", href, sidenav)
			}
			// The emitted href, sent back as a request path, names the doc.
			if body := getBody(t, h, href); !strings.Contains(body, ">"+c.title+"</h1>") {
				t.Errorf("GET %s did not serve %s:\n%s", href, c.name, body)
			}
		})
	}
}
