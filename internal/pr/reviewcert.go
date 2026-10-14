package pr

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// reviewCertGOOS is the platform reviewCertEnv judges, a seam for its tests.
var reviewCertGOOS = runtime.GOOS

// reviewCertDefault is the CA bundle macOS ships, a seam for tests.
var reviewCertDefault = "/etc/ssl/cert.pem"

// reviewCertEnv returns the CA variables a Claude review window needs on
// macOS: SSL_CERT_FILE and SSL_CERT_DIR each set, or, when forgectl has no
// usable value, listed to unset, so a stale value from the tmux server's
// environment cannot reach the window. Off macOS it returns nothing.
//
// Under Claude Code's Seatbelt sandbox, gh (Go) cannot reach the trust
// settings service, so every allow-listed gh read failed with "x509: OSStatus
// -26276"; SSL_CERT_FILE=/etc/ssl/cert.pem in the sandbox made the same read
// pass (measured on macOS, Claude Code 2.1.289, forgectl#1253). Pointing Go
// at a bundle file loosens nothing: the sandbox keeps trustd out of reach,
// which is the control enableWeakerNetworkIsolation would have given up.
//
// An SSL_CERT_FILE forgectl already holds wins when it names a readable
// regular file at an absolute path, so a corporate bundle keeps working.
// Otherwise /etc/ssl/cert.pem when it is readable. With neither, the variable
// is unset and the reads fail as before, with a warning. SSL_CERT_DIR, which
// Go also reads, is pinned to forgectl's own value when that is an absolute
// path to a directory, and unset otherwise.
func reviewCertEnv() (set, unset []string) {
	if reviewCertGOOS != "darwin" {
		return nil, nil
	}
	set, unset = reviewCertFileEnv()
	if v, ok := os.LookupEnv("SSL_CERT_DIR"); ok && v != "" && readableCertDir(v) {
		set = append(set, "SSL_CERT_DIR="+v)
	} else {
		unset = append(unset, "SSL_CERT_DIR")
	}
	return set, unset
}

// reviewCertFileEnv is reviewCertEnv's SSL_CERT_FILE half.
func reviewCertFileEnv() (set, unset []string) {
	if v, ok := os.LookupEnv("SSL_CERT_FILE"); ok && v != "" {
		if readableCertFile(v) {
			return []string{"SSL_CERT_FILE=" + v}, nil
		}
		slog.Warn("Ignoring SSL_CERT_FILE for the review window: it is not an absolute path to a readable file.",
			"path", v)
	}
	if readableCertFile(reviewCertDefault) {
		return []string{"SSL_CERT_FILE=" + reviewCertDefault}, nil
	}
	slog.Warn("The review's gh reads will likely fail TLS verification under the macOS sandbox: no CA bundle file to point them at.",
		"tried", reviewCertDefault)
	return nil, []string{"SSL_CERT_FILE"}
}

// readableCertDir reports whether path is absolute and is a directory.
func readableCertDir(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// readableCertFile reports whether path is absolute and opens as a regular
// file.
func readableCertFile(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	f, err := os.Open(path) //nolint:gosec // G304: a CA bundle path the operator set, or the system one; only opened to check it
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	return err == nil && info.Mode().IsRegular()
}
