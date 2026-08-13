//go:build !unix

package config

import (
	"fmt"
	"io"
	"os"
)

func ReadPath(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, ErrConfigNonRegular
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return data, nil
}
