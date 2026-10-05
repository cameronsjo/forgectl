package docs

import (
	"encoding/json"
	"os"
	"path"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Mermaid is vendored in the binary; its MIT notice must ship beside the
// font and KaTeX licenses, which the goreleaser archive entries glob.
func TestMermaidLicenseShips(t *testing.T) {
	b, err := os.ReadFile("assets/font-licenses/Mermaid-MIT.txt")
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.HasPrefix(s, "The MIT License") || !strings.Contains(s, "Sveidqvist") {
		t.Errorf("Mermaid-MIT.txt is not mermaid's MIT notice: %.80q", s)
	}
	y, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Archives []struct {
			ID    string `yaml:"id"`
			Files []any  `yaml:"files"`
		} `yaml:"archives"`
	}
	if err := yaml.Unmarshal(y, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Archives) == 0 {
		t.Fatal("no archives in .goreleaser.yaml")
	}
	for _, a := range cfg.Archives {
		found := false
		for _, f := range a.Files {
			if m, ok := f.(map[string]any); ok && m["src"] == "internal/docs/assets/font-licenses/*" {
				found = true
			}
		}
		if !found {
			t.Errorf("archive %q does not ship font-licenses/*", a.ID)
		}
	}
}

type mermaidProvenance struct {
	Files []struct {
		Version       string            `json:"version"`
		Notices       string            `json:"notices"`
		Dependencies  map[string]string `json:"dependencies"`
		BundleMarkers map[string]string `json:"bundle_markers"`
	} `json:"files"`
}

func readMermaidProvenance(t *testing.T) (version, notices string, deps, markers map[string]string) {
	t.Helper()
	pb, err := os.ReadFile("assets/provenance-mermaid.json")
	if err != nil {
		t.Fatal(err)
	}
	var prov mermaidProvenance
	if err := json.Unmarshal(pb, &prov); err != nil {
		t.Fatal(err)
	}
	if len(prov.Files) != 1 {
		t.Fatalf("provenance-mermaid.json: want 1 file entry, got %d", len(prov.Files))
	}
	f := prov.Files[0]
	return f.Version, f.Notices, f.Dependencies, f.BundleMarkers
}

func readMermaidNotices(t *testing.T) string {
	t.Helper()
	nb, err := os.ReadFile("assets/font-licenses/THIRD_PARTY_NOTICES-mermaid.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(nb)
}

// noticeSection returns the notices section for package pkg (a "\npkg@"
// heading through the next section rule), or "" when there is none. The
// generated index lists "  pkg@ver (license)", which never matches a heading.
func noticeSection(notices, pkg string) string {
	_, rest, ok := strings.Cut(notices, "\n"+pkg+"@")
	if !ok {
		return ""
	}
	if i := strings.Index(rest, "\n"+strings.Repeat("=", 78)+"\n"+"\n"); i >= 0 {
		// Skip the rule closing this section's own heading, then stop at the next heading.
		rest = rest[i+80:]
		if j := strings.Index(rest, "\n"+strings.Repeat("=", 78)+"\n"); j >= 0 {
			rest = rest[:j]
		}
	}
	return rest
}

// mermaid.min.js inlines dozens of libraries whose own notices the bundle's
// banner mostly omits. scripts/vendor-mermaid.sh generates
// THIRD_PARTY_NOTICES-mermaid.txt from the tree the bundle was built from; the
// direct dependency list recorded in provenance-mermaid.json is the floor the
// file must name, and the file must ride both archives through the
// font-licenses glob.
func TestMermaidThirdPartyNoticesShip(t *testing.T) {
	version, noticesRef, deps, _ := readMermaidProvenance(t)
	if len(deps) == 0 {
		t.Fatal("provenance-mermaid.json records no dependency list")
	}
	notices := readMermaidNotices(t)
	if !strings.Contains(noticesRef, "THIRD_PARTY_NOTICES-mermaid.txt") {
		t.Errorf("provenance does not point at the notices file: %q", noticesRef)
	}
	if want := "Third-party notices for mermaid " + version + " "; !strings.HasPrefix(notices, want) {
		t.Errorf("notices header does not name mermaid %s (provenance): %.80q", version, notices)
	}
	for dep := range deps {
		if noticeSection(notices, dep) == "" {
			t.Errorf("notices have no section for direct dependency %s", dep)
		}
	}
	// DOMPurify is dual-licensed: both texts must be present.
	dp := noticeSection(notices, "dompurify")
	for _, want := range []string{"Mozilla Public License", "Apache License"} {
		if !strings.Contains(dp, want) {
			t.Errorf("dompurify section lacks the %s text", want)
		}
	}

	y, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Archives []struct {
			ID    string `yaml:"id"`
			Files []any  `yaml:"files"`
		} `yaml:"archives"`
	}
	if err := yaml.Unmarshal(y, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Archives) < 2 {
		t.Fatalf("want both archives in .goreleaser.yaml, got %d", len(cfg.Archives))
	}
	for _, a := range cfg.Archives {
		ships := false
		for _, f := range a.Files {
			m, ok := f.(map[string]any)
			if !ok {
				continue
			}
			if src, _ := m["src"].(string); src != "" {
				if ok, _ := path.Match(src, "internal/docs/assets/font-licenses/THIRD_PARTY_NOTICES-mermaid.txt"); ok {
					ships = true
				}
			}
		}
		if !ships {
			t.Errorf("archive %q does not ship THIRD_PARTY_NOTICES-mermaid.txt", a.ID)
		}
	}
}

// Every library the bundle inlines must have a notice. bundle_markers in the
// provenance maps a literal string to the package it implies; the same check
// runs in scripts/vendor-mermaid.sh at generation time. The tree the notices
// came from once missed langium and chevrotain, both inlined, which is why
// they are required markers here.
func TestMermaidNoticesCoverBundle(t *testing.T) {
	_, _, _, markers := readMermaidProvenance(t)
	for marker, pkg := range map[string]string{
		"langium": "langium", "chevrotain": "chevrotain", "marked": "marked",
		"js-yaml": "js-yaml", "DOMPurify": "dompurify", "dayjs": "dayjs", "dagre-d3-es": "dagre-d3-es",
	} {
		if markers[marker] != pkg {
			t.Errorf("bundle_markers must map %q to %q, got %q", marker, pkg, markers[marker])
		}
	}
	js, err := os.ReadFile("assets/mermaid.min.js")
	if err != nil {
		t.Fatal(err)
	}
	notices := readMermaidNotices(t)
	found := 0
	for marker, pkg := range markers {
		if !strings.Contains(string(js), marker) {
			continue
		}
		found++
		if noticeSection(notices, pkg) == "" {
			t.Errorf("bundle contains %q but the notices have no section for %s", marker, pkg)
		}
	}
	if found < len(markers)/2 {
		t.Errorf("only %d of %d bundle markers occur in mermaid.min.js; the marker list is stale", found, len(markers))
	}
}
