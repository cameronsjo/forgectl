package sessions

import "testing"

func TestParseRunbook(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		content string
		want    RunbookRow
	}{
		{
			name:    "frontmatter supplies metadata",
			path:    "cadence/colima-split-brain.md",
			content: "---\ntitle: Colima split brain\nproject: hearth\ntype: field-report\nsession_id: abc-123\n---\n\n# Ignored heading\nbody text\n",
			want: RunbookRow{
				Path: "cadence/colima-split-brain.md", Slug: "colima-split-brain",
				Title: "Colima split brain", Project: "hearth", Type: "field-report",
				SessionID: "abc-123",
			},
		},
		{
			name:    "heading fallback when no frontmatter",
			path:    "forgectl/notes.md",
			content: "# Real Title\n\ncontent here\n",
			want: RunbookRow{
				Path: "forgectl/notes.md", Slug: "notes", Title: "Real Title",
				Project: "forgectl",
			},
		},
		{
			name:    "slug fallback when no title anywhere; root file has no project",
			path:    "orphan.md",
			content: "just prose\n",
			want:    RunbookRow{Path: "orphan.md", Slug: "orphan", Title: "orphan"},
		},
		{
			name:    "unclosed frontmatter treated as body",
			path:    "p/broken.md",
			content: "---\ntitle: never closed\n\n# Heading Wins\n",
			want:    RunbookRow{Path: "p/broken.md", Slug: "broken", Title: "Heading Wins", Project: "p"},
		},
		{
			name:    "quoted frontmatter values unwrap",
			path:    "p/q.md",
			content: "---\ntitle: \"Quoted Title\"\n---\nbody\n",
			want:    RunbookRow{Path: "p/q.md", Slug: "q", Title: "Quoted Title", Project: "p"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseRunbook(tt.path, tt.content, "test-machine")
			if got.Machine != "test-machine" {
				t.Errorf("machine = %q", got.Machine)
			}
			if got.FullText != tt.content {
				t.Errorf("full_text must be the verbatim document")
			}
			if got.Path != tt.want.Path || got.Slug != tt.want.Slug ||
				got.Title != tt.want.Title || got.Project != tt.want.Project ||
				got.Type != tt.want.Type || got.SessionID != tt.want.SessionID {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestScanRunbooksMissingRootIsEmpty(t *testing.T) {
	rows, err := ScanRunbooks("/nonexistent/corpus/root", "m")
	if err != nil {
		t.Fatalf("missing root must not error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected no rows, got %d", len(rows))
	}
}
