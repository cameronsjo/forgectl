//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package goalnative

import (
	"context"
	"errors"
	"io"
	"os"
)

func startSupervisor(string, string, *os.File, string, []string) (*os.Process, error) {
	return nil, errors.New("goal native supervisor is unsupported on this platform")
}
func RunHost(context.Context, string, io.Reader, io.Writer) error {
	return errors.New("goal native supervisor is unsupported on this platform")
}
func validateGuardianReceipts(string, string) error {
	return errors.New("goal native guardian is unsupported on this platform")
}
