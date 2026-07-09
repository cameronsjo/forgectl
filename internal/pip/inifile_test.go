package pip

// Test plan for inifile.go
//
// Parse/Serialize round-trip (Classification: pure logic / data transformer)
//   [x] Happy: real-shaped pip.conf (comments, [global]/[install], index-url,
//       extra-index-url) round-trips byte-for-byte
//   [x] Happy: trailing whitespace on a line is preserved
//   [x] Happy: no final newline is preserved
//   [x] Happy: a final newline is preserved
//   [x] Happy: multi-line (continuation) value round-trips
//   [x] Boundary: empty input round-trips to empty output
//   [x] Boundary: a single blank line round-trips
//   [x] Boundary: comment-only file round-trips
//   [x] Boundary: New() serializes to empty bytes
//
// classify (Classification: pure logic, exercised via Parse)
//   [x] Happy: section header sets Section for following lines
//   [x] Happy: entry key is lowercased regardless of source casing
//   [x] Happy: ';' is recognized as a comment prefix alongside '#'
//   [x] Happy: 'key: value' (colon delimiter) parses the same as 'key = value'
//   [x] Unhappy: a line with neither '=' nor ':' is inert (Comment), not a panic
//
// File.Remove / File.Restore (Classification: pure logic / stateful mutation)
//   [x] Happy: Remove comments out the matching entry line
//   [x] Happy: Remove comments out a multi-line entry's continuation lines too
//   [x] Happy: Remove only matches the requested section (same key elsewhere untouched)
//   [x] Happy: Remove returns 0 and leaves the file untouched when nothing matches
//   [x] Happy: Restore reverses Remove byte-for-byte (core reversibility property)
//   [x] Happy: Restore is idempotent (second call is a no-op, returns 0)

import (
	"bytes"
	"testing"
)

// ---- Parse/Serialize round-trip --------------------------------------------

func TestRoundTrip_RealShapedPipConf(t *testing.T) {
	src := `# Company-internal package index.
[global]
index-url = https://pypi.internal.example.com/simple
extra-index-url =
    https://pypi.org/simple
    https://test.pypi.org/simple

[install]
trusted-host = pypi.internal.example.com
`
	assertRoundTrip(t, src)
}

func TestRoundTrip_TrailingWhitespacePreserved(t *testing.T) {
	src := "[global]\nindex-url = https://pypi.org/simple   \n"
	assertRoundTrip(t, src)
}

func TestRoundTrip_NoFinalNewline(t *testing.T) {
	src := "[global]\nindex-url = https://pypi.org/simple"
	assertRoundTrip(t, src)
}

func TestRoundTrip_FinalNewline(t *testing.T) {
	src := "[global]\nindex-url = https://pypi.org/simple\n"
	assertRoundTrip(t, src)
}

func TestRoundTrip_ContinuationValue(t *testing.T) {
	src := "[global]\nextra-index-url =\n    https://a.example.com/simple\n    https://b.example.com/simple\n"
	assertRoundTrip(t, src)
}

func TestRoundTrip_EmptyInput(t *testing.T) {
	assertRoundTrip(t, "")
}

func TestRoundTrip_SingleBlankLine(t *testing.T) {
	assertRoundTrip(t, "\n")
}

func TestRoundTrip_CommentOnlyFile(t *testing.T) {
	assertRoundTrip(t, "# nothing configured yet\n; legacy comment style too\n")
}

func TestNewFile_SerializesToEmptyBytes(t *testing.T) {
	got := NewFile().Serialize()
	if len(got) != 0 {
		t.Errorf("NewFile().Serialize() = %q, want empty", got)
	}
}

func assertRoundTrip(t *testing.T, src string) {
	t.Helper()
	got := Parse([]byte(src)).Serialize()
	if !bytes.Equal(got, []byte(src)) {
		t.Errorf("round-trip mismatch:\n  input:  %q\n  output: %q", src, got)
	}
}

// ---- classify (via Parse) ---------------------------------------------------

func TestClassify_SectionHeaderSetsSectionForFollowingLines(t *testing.T) {
	f := Parse([]byte("[install]\ntrusted-host = pypi.internal.example.com\n"))
	entry := findEntry(t, f, "trusted-host")
	if entry.Section != "install" {
		t.Errorf("Section = %q, want %q", entry.Section, "install")
	}
}

func TestClassify_KeyIsLowercasedRegardlessOfSourceCasing(t *testing.T) {
	f := Parse([]byte("[global]\nIndex-URL = https://pypi.org/simple\n"))
	entry := findEntry(t, f, "index-url")
	if entry.Kind != KindEntry {
		t.Errorf("Kind = %v, want KindEntry", entry.Kind)
	}
}

func TestClassify_SemicolonIsCommentPrefix(t *testing.T) {
	f := Parse([]byte("; a semicolon comment\n"))
	if len(f.Lines) != 1 || f.Lines[0].Kind != KindComment {
		t.Fatalf("Lines = %+v, want one KindComment line", f.Lines)
	}
}

func TestClassify_ColonDelimiterParsesLikeEquals(t *testing.T) {
	f := Parse([]byte("[install]\ntrusted-host: pypi.internal.example.com\n"))
	entry := findEntry(t, f, "trusted-host")
	if entry.Kind != KindEntry {
		t.Errorf("Kind = %v, want KindEntry", entry.Kind)
	}
}

func TestClassify_LineWithNoDelimiterIsInertNotPanic(t *testing.T) {
	f := Parse([]byte("just some stray text\n"))
	if len(f.Lines) != 1 || f.Lines[0].Kind != KindComment {
		t.Fatalf("Lines = %+v, want one inert KindComment line", f.Lines)
	}
}

// findEntry returns the first KindEntry line whose Key matches want, failing
// the test if none is found.
func findEntry(t *testing.T, f *File, want string) Line {
	t.Helper()
	for _, l := range f.Lines {
		if l.Kind == KindEntry && l.Key == want {
			return l
		}
	}
	t.Fatalf("no KindEntry line with key %q in %+v", want, f.Lines)
	return Line{}
}

// ---- File.Remove / File.Restore --------------------------------------------

func TestRemove_CommentsOutMatchingEntry(t *testing.T) {
	f := Parse([]byte("[global]\nindex-url = https://pypi.internal.example.com/simple\n"))
	n := f.Remove("global", "index-url")
	if n != 1 {
		t.Fatalf("Remove returned %d, want 1", n)
	}
	out := string(f.Serialize())
	if !bytes.Contains([]byte(out), []byte(removedMarker+"index-url = https://pypi.internal.example.com/simple")) {
		t.Errorf("removed line not marked as expected: %q", out)
	}
}

func TestRemove_CommentsOutContinuationLinesToo(t *testing.T) {
	src := "[global]\nextra-index-url =\n    https://a.example.com/simple\n    https://b.example.com/simple\n"
	f := Parse([]byte(src))
	n := f.Remove("global", "extra-index-url")
	if n != 1 {
		t.Fatalf("Remove returned %d, want 1", n)
	}
	for i, l := range f.Lines {
		if i == 0 { // [global] header untouched
			continue
		}
		if !hasRemovedMarker(l.Raw) {
			t.Errorf("line %d not marked removed: %q", i, l.Raw)
		}
	}
}

func TestRemove_OnlyMatchesRequestedSection(t *testing.T) {
	src := "[global]\nindex-url = https://internal.example.com/simple\n\n[install]\nindex-url = https://other.example.com/simple\n"
	f := Parse([]byte(src))
	n := f.Remove("global", "index-url")
	if n != 1 {
		t.Fatalf("Remove returned %d, want 1", n)
	}
	installEntry := findEntry(t, f, "index-url")
	if installEntry.Section != "install" {
		t.Fatalf("expected the surviving entry to be in [install], got %+v", installEntry)
	}
}

func TestRemove_NoMatch_ReturnsZeroAndLeavesFileUntouched(t *testing.T) {
	src := "[global]\nindex-url = https://pypi.org/simple\n"
	f := Parse([]byte(src))
	n := f.Remove("global", "extra-index-url")
	if n != 0 {
		t.Fatalf("Remove returned %d, want 0", n)
	}
	if string(f.Serialize()) != src {
		t.Errorf("file mutated on no-match Remove: %q", f.Serialize())
	}
}

func TestRestore_ReversesRemoveByteForByte(t *testing.T) {
	src := `[global]
index-url = https://pypi.internal.example.com/simple
extra-index-url =
    https://pypi.org/simple
    https://test.pypi.org/simple
`
	f := Parse([]byte(src))
	if n := f.Remove("global", "index-url"); n != 1 {
		t.Fatalf("Remove(index-url) = %d, want 1", n)
	}
	if n := f.Remove("global", "extra-index-url"); n != 1 {
		t.Fatalf("Remove(extra-index-url) = %d, want 1", n)
	}
	if n := f.Restore(); n == 0 {
		t.Fatalf("Restore returned 0, want > 0")
	}
	if got := string(f.Serialize()); got != src {
		t.Errorf("restore(remove(x)) != x:\n  want: %q\n  got:  %q", src, got)
	}
}

func TestRestore_IsIdempotent(t *testing.T) {
	src := "[global]\nindex-url = https://pypi.org/simple\n"
	f := Parse([]byte(src))
	f.Remove("global", "index-url")
	f.Restore()
	if n := f.Restore(); n != 0 {
		t.Errorf("second Restore() = %d, want 0", n)
	}
}

func hasRemovedMarker(s string) bool {
	return len(s) >= len(removedMarker) && s[:len(removedMarker)] == removedMarker
}
