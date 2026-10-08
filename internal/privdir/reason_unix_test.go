//go:build unix

package privdir

import (
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// TestUnsafe_ReasonIsBounded is forgectl#864: a refusal quoting an OS error
// that carries a very long path, as createBase's os.Mkdir error does, renders
// bounded, with the path cut in the middle so the errno after it still reads.
//
// Mutations that turn it red: render the reason with SafeLine instead of
// SafeLineMax and skip the termsafe.Error argument rendering (unbounded), or
// skip only the termsafe.Error rendering (the cap then eats the errno).
func TestUnsafe_ReasonIsBounded(t *testing.T) {
	long := "/state/" + strings.Repeat("a\u202e", 3000) + "/forgectl"
	err := unsafe("create base: %s", &os.PathError{Op: "mkdir", Path: long, Err: unix.ENAMETOOLONG})
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("unsafe() = %v, want ErrUnsafe", err)
	}
	got := err.Error()
	if n := utf8.RuneCountInString(got); n > reasonMaxRunes+100 {
		t.Errorf("refusal is %d runes, want at most about %d: %.200q", n, reasonMaxRunes, got)
	}
	if !strings.Contains(got, unix.ENAMETOOLONG.Error()) {
		t.Errorf("refusal lost the errno after the path: %q", got)
	}
	if strings.ContainsRune(got, '\u202e') {
		t.Errorf("refusal carries a raw bidi override: %q", got)
	}
}
