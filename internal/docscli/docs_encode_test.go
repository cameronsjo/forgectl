// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package docscli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/module"
)

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

var errWriteFailed = errors.New("write failed")

func renderCmd(t *testing.T, f func(cmd *cobra.Command) error) (stdout, stderr string) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "test"}
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	if err := f(cmd); err != nil {
		t.Fatalf("renderCmd: %v", err)
	}
	return outBuf.String(), errBuf.String()
}

func TestPrintDocsSearch_EncodeFailuresKeepDocsShape(t *testing.T) {
	var stderr bytes.Buffer
	c := &cobra.Command{Use: "search"}
	c.SetOut(failWriter{})
	c.SetErr(&stderr)
	err := printDocsSearch(c, docspkg.SearchResponse{}, true)
	if ExitCode(err) != 1 {
		t.Errorf("stdout failure exit = %d, want 1", ExitCode(err))
	}
	if _, ok := err.(*silentCodedError); !ok {
		t.Errorf("stdout failure = %T, want *silentCodedError", err)
	}
	var obj docsErrorJSON
	if jerr := json.Unmarshal(stderr.Bytes(), &obj); jerr != nil || obj.Code != 1 || !strings.Contains(obj.Error, "write refused") {
		t.Errorf("stderr = %q (%v), want one docs object with code 1", stderr.String(), jerr)
	}

	var stdout bytes.Buffer
	c = &cobra.Command{Use: "search"}
	c.SetOut(&stdout)
	c.SetErr(failWriter{})
	err = printDocsSearch(c, docspkg.SearchResponse{Errors: []docspkg.SearchError{{Root: "r", Message: "m"}}}, true)
	if ExitCode(err) != 1 {
		t.Errorf("stderr failure exit = %d, want 1", ExitCode(err))
	}
	if _, ok := err.(*silentCodedError); !ok {
		t.Errorf("stderr failure = %T, want *silentCodedError: the response is already on stdout", err)
	}
	if stdout.Len() == 0 {
		t.Error("the response never reached stdout")
	}
}

func TestDocsList_StdoutEncodeFailureKeepsDocsShape(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.md"), []byte("# Page"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := newDocsListCmd(module.Deps{})
	cmd.SetOut(failWriter{})
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json", dir})
	err := cmd.ExecuteContext(context.Background())
	if ExitCode(err) != 1 {
		t.Errorf("exit = %d (err %v), want 1", ExitCode(err), err)
	}
	if _, ok := err.(*silentCodedError); !ok {
		t.Errorf("err = %T, want *silentCodedError", err)
	}
	var obj docsErrorJSON
	if jerr := json.Unmarshal(stderr.Bytes(), &obj); jerr != nil || obj.Code != 1 || !strings.Contains(obj.Error, "write refused") {
		t.Errorf("stderr = %q (%v), want one docs object with code 1", stderr.String(), jerr)
	}
}

func TestWrapDocsTokenDescriptorError_QuotesExactlyOnce(t *testing.T) {
	file := &injectedDocsTokenFile{
		reader:   &partialErrorReader{data: nil, err: errors.New("injected read failure")},
		closeErr: errors.New("injected close failure"),
	}
	_, err := readDocsTokenFile("/safe/token", file)
	if err == nil {
		t.Fatal("a failing read and close must produce an error")
	}
	text := err.Error()
	if got := strings.Count(text, `"/safe/token"`); got != 2 {
		t.Fatalf("path quoted %d time(s), want exactly once per joined failure (read + close): %q", got, text)
	}
	for _, doubled := range []string{`\"`, `""`, `"\"`} {
		if strings.Contains(text, doubled) {
			t.Fatalf("path was quoted more than once (%q): %q", doubled, text)
		}
	}
}
