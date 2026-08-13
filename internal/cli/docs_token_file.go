package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const maxDocsTokenFileBytes = 4096

// openedDocsTokenFile is the already-open, identity-validated descriptor used
// for the bounded read. Keeping the reader and closer together prevents a
// pathname reopen between validation and acquisition.
type openedDocsTokenFile interface {
	io.Reader
	Close() error
}

func acquireDocsTokenFile(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("token file path must be absolute and clean: %s", safeDocsTokenPath(path))
	}
	file, err := openDocsTokenFile(path)
	if err != nil {
		return "", err
	}
	return readDocsTokenFile(path, file)
}

func readDocsTokenFile(path string, file openedDocsTokenFile) (string, error) {
	displayPath := safeDocsTokenPath(path)
	raw, readErr := io.ReadAll(io.LimitReader(file, maxDocsTokenFileBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		var failures []error
		if readErr != nil {
			failures = append(failures, fmt.Errorf("read token file %s: %w", displayPath, readErr))
		}
		if closeErr != nil {
			failures = append(failures, fmt.Errorf("close token file %s: %w", displayPath, closeErr))
		}
		return "", errors.Join(failures...)
	}
	if len(raw) > maxDocsTokenFileBytes {
		return "", fmt.Errorf("token file is too large: %s", displayPath)
	}

	if len(raw) >= 2 && raw[len(raw)-2] == '\r' && raw[len(raw)-1] == '\n' {
		raw = raw[:len(raw)-2]
	} else if len(raw) >= 1 && raw[len(raw)-1] == '\n' {
		raw = raw[:len(raw)-1]
	}
	if !validDocsBearerToken(raw) {
		return "", fmt.Errorf("token file contains an invalid bearer token: %s", displayPath)
	}
	return string(raw), nil
}

func safeDocsTokenPath(path string) string { return termsafe.Sanitize(path) }

func wrapDocsTokenPathError(operation, path string, err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	return fmt.Errorf("%s token file %s: %w", operation, safeDocsTokenPath(path), err)
}

// validDocsBearerToken implements RFC 6750's b64token grammar byte-for-byte:
// one or more ALPHA / DIGIT / -._~+/, followed only by optional '=' padding.
func validDocsBearerToken(token []byte) bool {
	if len(token) == 0 {
		return false
	}
	i := 0
	for i < len(token) && isDocsBearerBaseByte(token[i]) {
		i++
	}
	if i == 0 {
		return false
	}
	for i < len(token) && token[i] == '=' {
		i++
	}
	return i == len(token)
}

func isDocsBearerBaseByte(b byte) bool {
	return b >= 'a' && b <= 'z' ||
		b >= 'A' && b <= 'Z' ||
		b >= '0' && b <= '9' ||
		b == '-' || b == '.' || b == '_' || b == '~' || b == '+' || b == '/'
}
