package history

import (
	"strings"
	"testing"
)

// FuzzParse asserts Parse's contract over arbitrary bytes: it terminates, it
// never panics, a refusal returns no entries, and a success returns only
// non-empty commands. The line-walking fold loop is the reason this exists —
// it replaced strings.Split with hand-rolled scanning so the entry cap could
// bound allocation, and a hand-rolled loop over untrusted text is exactly the
// thing to fuzz rather than reason about.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"",
		"\n",
		"\n\n",
		"echo one",
		"echo one\n",
		"echo one\necho two\n",
		": 1690000000:0;echo one\n",
		": 1690000000:0;echo one\n: 1690000060:5;echo two\n",
		": 1690000000:0;line one\\\nline two\n",
		": 1690000000:0;ends in a backslash\\\n: 1690000060:0;echo two\n",
		"echo one\\",
		"\\\n",
		": > /tmp/plain-colon-builtin\n",
		"echo one\n: 1690000000:0;echo hi\necho two\n",
		": nope:0;echo one\n: 1690000060:0;echo two\n",
		": 1690000000:0;\n",
		": :;x\n",
		string([]byte{0x83, 0xE3, 0x83, 0x89, '\n'}),
		string([]byte{0x83}),
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	// The fold bound needs its own seeds: reaching it takes thousands of
	// consecutive continuation lines, and a single mutated line without a
	// trailing backslash resets the run — so mutation will not find it from
	// the short seeds above. One seed sits just under the bound and one just
	// over, so the fuzzer can mutate across the boundary rather than only
	// past it.
	f.Add([]byte(strings.Repeat("a\\\n", MaxContinuationLines-1) + "done\n"))
	f.Add([]byte(strings.Repeat("a\\\n", MaxContinuationLines+1)))

	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := Parse(data)
		if err != nil {
			if entries != nil {
				t.Fatalf("Parse refused with %v but returned %d entries", err, len(entries))
			}
			return
		}
		if len(entries) == 0 {
			t.Fatal("Parse succeeded with no entries; an empty history must refuse")
		}
		for i, entry := range entries {
			if entry.Command == "" {
				t.Fatalf("entries[%d] has an empty command", i)
			}
			if entry.Elapsed < 0 {
				t.Fatalf("entries[%d] has a negative elapsed %v", i, entry.Elapsed)
			}
		}
	})
}

// TestFoldRecords_LineWalkingEdgeCases pins the fold loop's termination and
// record boundaries on the inputs a hand-rolled scanner gets wrong: no trailing
// newline, a trailing newline, consecutive newlines, and a final continuation.
func TestFoldRecords_LineWalkingEdgeCases(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		want  []string
		fails bool
	}{
		{name: "single line, no trailing newline", text: "echo one", want: []string{"echo one"}},
		{name: "two lines, no trailing newline", text: "echo one\necho two", want: []string{"echo one", "echo two"}},
		{name: "blank line between", text: "echo one\n\necho two", want: []string{"echo one", "", "echo two"}},
		{name: "continuation", text: "echo one\\\ncontinued", want: []string{"echo one\ncontinued"}},
		{name: "continuation broken by a header", text: "echo one\\\n: 1690000000:0;echo two", want: []string{"echo one\\", ": 1690000000:0;echo two"}},
		{name: "final line is a continuation", text: "echo one\\", fails: true},
		{name: "lone newline", text: "\n", want: []string{"", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records, err := foldRecords(tc.text)
			if tc.fails {
				if err == nil {
					t.Fatalf("foldRecords accepted %q; an unterminated continuation must refuse", tc.text)
				}
				return
			}
			if err != nil {
				t.Fatalf("foldRecords(%q): unexpected error: %v", tc.text, err)
			}
			var got []string
			for _, record := range records {
				got = append(got, record.text)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("foldRecords(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}
