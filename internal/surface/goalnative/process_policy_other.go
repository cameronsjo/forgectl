//go:build !darwin

package goalnative

import "errors"

func confinedCommand([]string) ([]string, error) {
	return nil, errors.New("strict process containment is unsupported on this platform")
}
