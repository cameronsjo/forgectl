package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDocsToken_SourcePolicy(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("Az09-._~+/===\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, addr := range []string{"127.0.0.1:0", "0.0.0.0:3590"} {
		got, err := resolveDocsToken(tokenPath, addr)
		if err != nil {
			t.Fatalf("resolveDocsToken(file, %q): %v", addr, err)
		}
		if got.value != "Az09-._~+/===" || got.source != docsTokenFromFile {
			t.Fatalf("resolveDocsToken(file, %q) = %+v, want exact file token/source", addr, got)
		}
	}

	loopback, err := resolveDocsToken("", "127.0.0.1:0")
	if err != nil || loopback.value != "" || loopback.source != docsTokenNone {
		t.Fatalf("resolveDocsToken(loopback) = (%+v, %v), want no token", loopback, err)
	}
	exposed, err := resolveDocsToken("", "0.0.0.0:3590")
	if err != nil || exposed.value == "" || exposed.source != docsTokenGenerated {
		t.Fatalf("resolveDocsToken(exposed) = (%+v, %v), want generated token", exposed, err)
	}
}

func TestAcquireDocsTokenFile_ErrorPathIsTerminalSafe(t *testing.T) {
	_, err := acquireDocsTokenFile(filepath.Join(t.TempDir(), "bad\n\u202epath"))
	if err == nil {
		t.Fatal("acquireDocsTokenFile on missing path returned nil error")
	}
	if strings.ContainsAny(err.Error(), "\n\r") || strings.ContainsRune(err.Error(), '\u202e') {
		t.Fatalf("error retained terminal control characters: %q", err)
	}
}

func TestWrapDocsTokenPathError_RedactsWrappedPathAndPreservesIdentity(t *testing.T) {
	rawPath := "/tmp/bad\n\u202epath"
	err := wrapDocsTokenPathError("open", rawPath, &os.PathError{Op: "open", Path: rawPath, Err: os.ErrNotExist})
	if strings.ContainsAny(err.Error(), "\n\r") || strings.ContainsRune(err.Error(), '\u202e') {
		t.Fatalf("wrapped error retained terminal controls: %q", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrapped error lost errors.Is identity: %v", err)
	}
}

type injectedDocsTokenFile struct {
	reader     io.Reader
	closeErr   error
	closeCount int
}

func (f *injectedDocsTokenFile) Read(p []byte) (int, error) { return f.reader.Read(p) }
func (f *injectedDocsTokenFile) Close() error {
	f.closeCount++
	return f.closeErr
}

type partialErrorReader struct {
	data []byte
	err  error
	done bool
}

func (r *partialErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), r.err
}

func TestReadDocsTokenFile_RawSizeAndValidationOrder(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "4096 token bytes", raw: strings.Repeat("A", 4096), want: strings.Repeat("A", 4096)},
		{name: "4095 plus LF", raw: strings.Repeat("A", 4095) + "\n", want: strings.Repeat("A", 4095)},
		{name: "4094 plus CRLF", raw: strings.Repeat("A", 4094) + "\r\n", want: strings.Repeat("A", 4094)},
		{name: "4097 token bytes", raw: strings.Repeat("A", 4097), wantErr: "too large"},
		{name: "4096 plus LF", raw: strings.Repeat("A", 4096) + "\n", wantErr: "too large"},
		{name: "4095 plus CRLF", raw: strings.Repeat("A", 4095) + "\r\n", wantErr: "too large"},
		{name: "oversize before early grammar", raw: ":" + strings.Repeat("A", 4096), wantErr: "too large"},
		{name: "oversize before late grammar", raw: strings.Repeat("A", 4096) + ":", wantErr: "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := &injectedDocsTokenFile{reader: strings.NewReader(tt.raw)}
			got, err := readDocsTokenFile("/safe/token", file)
			if tt.wantErr == "" {
				if err != nil || got != tt.want {
					t.Fatalf("readDocsTokenFile = (%q, %v), want (%q, nil)", got, err, tt.want)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || got != "" {
					t.Fatalf("readDocsTokenFile = (%q, %v), want empty token and %q error", got, err, tt.wantErr)
				}
				if strings.Contains(err.Error(), tt.raw) {
					t.Fatalf("error leaked token bytes: %v", err)
				}
			}
			if file.closeCount != 1 {
				t.Fatalf("close count = %d, want 1", file.closeCount)
			}
		})
	}
}

func TestReadDocsTokenFile_B64TokenGrammarAndEOL(t *testing.T) {
	valid := []string{
		"deadbeef", "Az09-._~+/", "a=", "Az09-._~+/===",
	}
	for _, token := range valid {
		for _, eol := range []string{"", "\n", "\r\n"} {
			name := token + strings.ReplaceAll(strings.ReplaceAll(eol, "\r", "CR"), "\n", "LF")
			t.Run("valid_"+name, func(t *testing.T) {
				file := &injectedDocsTokenFile{reader: strings.NewReader(token + eol)}
				got, err := readDocsTokenFile("/safe/token", file)
				if err != nil || got != token {
					t.Fatalf("readDocsTokenFile = (%q, %v), want (%q, nil)", got, err, token)
				}
			})
		}
	}

	invalid := []string{
		"", "=", "==", "=a", "a=b", "a==b", ":", " a", "a ", "a\tb",
		"a\x00b", "a\x7fb", "a\x01b", "a\u00e9", "a!b", "\n", "\r\n",
		"a\nb", "a\rb", "a\n\n", "a\r\n\r\n", "a\r",
	}
	for i, raw := range invalid {
		t.Run("invalid_"+strings.Repeat("x", i+1), func(t *testing.T) {
			file := &injectedDocsTokenFile{reader: strings.NewReader(raw)}
			got, err := readDocsTokenFile("/safe/token", file)
			if err == nil || !strings.Contains(err.Error(), "invalid bearer token") || got != "" {
				t.Fatalf("readDocsTokenFile(%q) = (%q, %v), want redacted invalid-token error", raw, got, err)
			}
			if err.Error() != "token file contains an invalid bearer token: /safe/token" {
				t.Fatalf("error = %q, want fixed class and safe path only", err)
			}
		})
	}
}

func TestReadDocsTokenFile_ReadAndCloseFailuresJoinWithoutLeak(t *testing.T) {
	readErr := errors.New("injected read failure")
	closeErr := errors.New("injected close failure")
	const sentinel = "LEAK-ME-NOT"

	tests := []struct {
		name      string
		reader    io.Reader
		closeErr  error
		wantRead  bool
		wantClose bool
	}{
		{name: "partial read", reader: &partialErrorReader{data: []byte(sentinel), err: readErr}, wantRead: true},
		{name: "close", reader: strings.NewReader("valid"), closeErr: closeErr, wantClose: true},
		{name: "read and close", reader: &partialErrorReader{data: []byte(sentinel), err: readErr}, closeErr: closeErr, wantRead: true, wantClose: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := &injectedDocsTokenFile{reader: tt.reader, closeErr: tt.closeErr}
			got, err := readDocsTokenFile("/safe/token", file)
			if got != "" || err == nil {
				t.Fatalf("readDocsTokenFile = (%q, %v), want empty token and error", got, err)
			}
			if errors.Is(err, readErr) != tt.wantRead || errors.Is(err, closeErr) != tt.wantClose {
				t.Fatalf("error identity = %v, want read=%v close=%v", err, tt.wantRead, tt.wantClose)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("error leaked partial token bytes: %v", err)
			}
			if file.closeCount != 1 {
				t.Fatalf("close count = %d, want 1", file.closeCount)
			}
		})
	}
}
