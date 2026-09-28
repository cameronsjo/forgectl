package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/pr"
)

func TestFindingsListJSON_EmptyIsArray(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFindingsListJSON(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(buf.String()); got != "[]" {
		t.Errorf("empty findings = %s, want []", got)
	}
}

func TestFindingsListJSON_Fields(t *testing.T) {
	mod := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := writeFindingsListJSON(&buf, []pr.FindingsEntry{{Path: "/f/a", ModTime: mod, Size: 2048}}); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	if len(rows) != 1 || len(rows[0]) != 3 {
		t.Fatalf("rows = %v, want one row of 3 fields", rows)
	}
	if rows[0]["path"] != "/f/a" || rows[0]["modified_at"] != "2026-09-01T12:00:00Z" || rows[0]["size_bytes"] != 2048.0 {
		t.Errorf("row = %v", rows[0])
	}
}
