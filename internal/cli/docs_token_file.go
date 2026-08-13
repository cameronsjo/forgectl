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
			failures = append(failures, wrapDocsTokenDescriptorError("read", displayPath, readErr))
		}
		if closeErr != nil {
			failures = append(failures, wrapDocsTokenDescriptorError("close", displayPath, closeErr))
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

// wrapDocsTokenDescriptorError removes path-bearing wrappers before an error
// reaches the terminal. *os.File operations reuse the raw os.NewFile name in
// PathError, so a sanitized outer prefix alone is not sufficient.
func wrapDocsTokenDescriptorError(operation, path string, err error) error {
	return fmt.Errorf("%s token file %s: %w", operation, safeDocsTokenPath(path), docsTokenErrorCause(err))
}

func docsTokenErrorCause(err error) error {
	//nolint:errorlint // Direct unwrapping deliberately removes path-bearing wrappers; errors.As would retain them.
	switch typed := err.(type) {
	case *os.PathError:
		return docsTokenErrorCause(typed.Err)
	case *os.LinkError:
		return docsTokenErrorCause(typed.Err)
	case interface{ Unwrap() []error }:
		children := typed.Unwrap()
		sanitized := make([]error, 0, len(children))
		for _, child := range children {
			sanitized = append(sanitized, docsTokenErrorCause(child))
		}
		return errors.Join(sanitized...)
	case interface{ Unwrap() error }:
		child := typed.Unwrap()
		if docsTokenErrorHasPathFields(child) {
			return docsTokenErrorCause(child)
		}
		return err
	default:
		return err
	}
}

func closeInvalidDocsTokenFile(file *os.File, validationErr error) error {
	if closeErr := file.Close(); closeErr != nil {
		return errors.Join(validationErr, wrapDocsTokenDescriptorError("close", file.Name(), closeErr))
	}
	return validationErr
}

func docsTokenErrorHasPathFields(err error) bool {
	var pathErr *os.PathError
	var linkErr *os.LinkError
	return errors.As(err, &pathErr) || errors.As(err, &linkErr)
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
