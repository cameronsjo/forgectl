package tmuxesc

import "testing"

// TestDirOperand pins the -c spelling forgectl#839 needs: the format
// escape and the trailing-';' escape together, each alone, and an ordinary
// path unchanged. The real-tmux proof is internal/tmux's
// TestStartDirectoryIsNotFormatExpandedIsolated.
//
// Mutation that turns it red: drop either escape from DirOperand.
func TestDirOperand(t *testing.T) {
	for in, want := range map[string]string{
		"/plain/dir":        "/plain/dir",
		"/a#(touch m)":      "/a##(touch m)",
		"/a#{session_name}": "/a##{session_name}",
		"/a##b":             "/a####b",
		"/a#[x":             "/a#[x",
		"/semi;":            `/semi\;`,
		"/both#;":           `/both##\;`,
	} {
		if got := DirOperand(in); got != want {
			t.Errorf("DirOperand(%q) = %q, want %q", in, got, want)
		}
	}
}
