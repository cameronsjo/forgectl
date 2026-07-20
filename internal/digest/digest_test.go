package digest

import "testing"

func TestSHA256_CanonicalForm(t *testing.T) {
	// Known SHA-256 of the empty input, in the canonical "sha256:<hex>" form.
	const wantEmpty = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := SHA256(nil); got != wantEmpty {
		t.Errorf("SHA256(nil) = %q, want %q", got, wantEmpty)
	}
	if got := SHA256([]byte{}); got != wantEmpty {
		t.Errorf("SHA256(empty) = %q, want %q", got, wantEmpty)
	}

	// Distinct inputs hash distinctly, and the prefix/casing are stable.
	a, b := SHA256([]byte("a")), SHA256([]byte("b"))
	if a == b {
		t.Error("distinct inputs must hash distinctly")
	}
	for _, h := range []string{a, b} {
		if len(h) != len("sha256:")+64 || h[:7] != "sha256:" {
			t.Errorf("non-canonical form: %q", h)
		}
	}
}
