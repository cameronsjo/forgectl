package pr

import (
	"errors"
	"io/fs"
)

// errTextUnavailable is the categorical text safeErrString gives an error
// whose Error method panicked. It says what happened without repeating the
// panic value.
const errTextUnavailable = "error text unavailable: its Error method panicked " +
	"(an os.Root operation racing a symlink swap is the known cause)"

// safeErrString is err.Error() for an error whose Error method may panic.
// Go 1.26's os.Root.RemoveAll can leak its internal errSymlink, wrapped in a
// *fs.PathError, when a directory it is walking is swapped for a symlink
// (forgectl#764), and errSymlink's Error method is a panic. fmt and slog
// recover such a panic, but a direct err.Error() does not, so a same-uid
// racer could crash `pr findings cleanup --apply` between its intent row and
// its completion row.
//
// A panicking error gets the categorical errTextUnavailable, prefixed with
// the Op and Path of the *fs.PathError that carries it when there is one:
// those are plain strings, so reading them cannot panic. A nil err is "".
func safeErrString(err error) string {
	s, _ := renderErr(err)
	return s
}

// renderableErr returns err unchanged when its Error method returns, and a
// plain error carrying safeErrString's text when it panics, so the result can
// be wrapped, returned, and printed by any caller. It drops the panicking
// error from the chain, since whatever errors.As found there would panic
// again.
func renderableErr(err error) error {
	if err == nil {
		return nil
	}
	if s, ok := renderErr(err); !ok {
		return errors.New(s)
	}
	return err
}

// renderErr is err.Error() and true, or the categorical text and false when
// Error panics.
func renderErr(err error) (s string, ok bool) {
	if err == nil {
		return "", true
	}
	defer func() {
		if recover() != nil {
			s, ok = unrenderableErrText(err), false
		}
	}()
	return err.Error(), true
}

func unrenderableErrText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + " " + pe.Path + ": " + errTextUnavailable
	}
	return errTextUnavailable
}
