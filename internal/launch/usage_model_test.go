package launch

import (
	"encoding/json"
	"strings"
	"testing"
)

// hostileModel is a model label carrying a raw terminal escape. It is built
// from a rune rather than written as a literal so this source file contains no
// control byte of its own.
func hostileModel() string {
	return "opus" + string(rune(0x1b)) + "[2Kfake"
}

func TestUsageEventV1_ModelRepresentation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness string
		model   string
		// fragment pins the literal JSON spelling where it is part of the
		// contract. The hostile case leaves it empty: the escape spelling is
		// the encoder's business, while the round-trip below is not.
		fragment string
	}{
		{"claude default", "claude", "opus", `"model":"opus"`},
		{"codex native default", "codex", "", `"model":""`},
		{"pi native default", "pi", "", `"model":""`},
		{"custom deployment label", "claude", "internal-deploy-7", `"model":"internal-deploy-7"`},
		{"hostile label survives without a raw control byte", "claude", hostileModel(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := sampleUsageEvent()
			ev.Harness = tc.harness
			ev.Model = tc.model
			row, err := EncodeUsageEvent(ev)
			if err != nil {
				t.Fatalf("EncodeUsageEvent: %v", err)
			}
			if tc.fragment != "" && !strings.Contains(string(row), tc.fragment) {
				t.Fatalf("row %s does not contain %s", row, tc.fragment)
			}

			// The model round-trips byte-for-byte: JSON string encoding may
			// escape a control byte, but it must never drop, replace, or
			// truncate one — the stored value is the operator's, unaltered.
			var back UsageEventV1
			if err := json.Unmarshal(row, &back); err != nil {
				t.Fatalf("decode round-trip: %v", err)
			}
			if back.Model != tc.model {
				t.Fatalf("round-tripped model = %q, want %q", back.Model, tc.model)
			}

			// No raw control byte reaches the file, so `cat`ing the store
			// cannot execute a sequence the operator never typed.
			for _, b := range row[:len(row)-1] {
				if b < 0x20 || b == 0x7f {
					t.Fatalf("row carries a raw control byte %#x: %q", b, row)
				}
			}
			if !strings.HasSuffix(string(row), "\n") || strings.Count(string(row), "\n") != 1 {
				t.Fatalf("row %q must end in exactly one newline", row)
			}
		})
	}
}
