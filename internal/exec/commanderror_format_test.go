package exec

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestCommandErrorFormatsRedacted pins #926: a *CommandError keeps a URL
// credential in its exported Args, Stderr and Output fields by design, so
// every way fmt and slog render one must go through the same redaction as
// Error(). Before Format and GoString existed, %#v and any verb fmt does not
// route to Error() (%d, %t) printed the fields verbatim.
//
// It also pins that the verbs fmt already routed to Error() render exactly
// what they did before: Error()'s text under the directive's own flags,
// width and precision.
//
// Mutations that turn it red: delete Format (the %#v, %d and %t rows print
// the fields); in GoString, render Stderr, Output or Err's text without
// redact.Text, Output through redact.Text rather than withheld, or Args
// without renderArgs; in Format, drop the %#v case (it
// falls to the Error() verbs and the GoString shape row fails); make LogValue
// return slog.AnyValue(*e) (the slog rows print the fields).
func TestCommandErrorFormatsRedacted(t *testing.T) {
	const secret = "ghp_fakeFormatToken0123456789" //nolint:gosec // G101: a fake token the redactor must hide
	// Stdout that is no URL: redact.Text keeps it, so only withholding
	// Output whole keeps it out of %#v.
	const plainSecret = "hunter2plainStdoutSecret"
	url := "https://x-access-token:" + secret + "@example.invalid/owner/repo"
	inner := &CommandError{Name: "git", Args: []string{"fetch", url}, Stderr: "fatal: " + url, ExitCode: 128, Err: errors.New("exit status 128")}
	e := &CommandError{
		Name:     "git",
		Args:     []string{"clone", url},
		Stderr:   "fatal: unable to access '" + url + "'",
		Output:   "Cloning into " + url + "\nclipboard: " + plainSecret,
		ExitCode: 128,
		Err:      fmt.Errorf("remote %s: %w", url, inner),
	}
	// The fixture must carry the secret where the fields are read, or a
	// clean render proves nothing.
	if !strings.Contains(e.Args[1]+e.Stderr+e.Output+e.Err.Error(), secret) {
		t.Fatal("the fixture holds no secret in its fields")
	}
	leaks := func(s string) bool {
		return strings.Contains(s, secret) || strings.Contains(s, plainSecret) ||
			strings.Contains(s, hex.EncodeToString([]byte(secret))) ||
			strings.Contains(s, strings.ToUpper(hex.EncodeToString([]byte(secret))))
	}

	renders := map[string]string{}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%10v", "%-8s", "%.40s"} {
		renders["Sprintf "+verb] = fmt.Sprintf(verb, e)
	}
	renders["Sprint"] = fmt.Sprint(e)
	renders["Sprintln"] = fmt.Sprintln(e)
	renders["wrapped %+v"] = fmt.Sprintf("%+v", fmt.Errorf("ctx: %w", e))
	renders["in a struct %+v"] = fmt.Sprintf("%+v", struct{ E *CommandError }{e})
	renders["in a struct %#v"] = fmt.Sprintf("%#v", struct{ E *CommandError }{e})
	for name, h := range map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		var b bytes.Buffer
		l := slog.New(h(&b))
		l.Info("failed", slog.Any("error", e))
		l.Info("failed", "error", e)
		l.Info("failed", slog.Group("g", slog.Any("error", e)))
		renders["slog "+name] = b.String()
	}
	for name, got := range renders {
		if leaks(got) {
			t.Errorf("%s renders the credential: %s", name, got)
		}
	}

	// The Error() verbs keep their bytes.
	want := e.Error()
	for verb, w := range map[string]string{
		"%v": want, "%+v": want, "%s": want, "%q": fmt.Sprintf("%q", want),
		"%x": fmt.Sprintf("%x", want), "%X": fmt.Sprintf("%X", want),
		"%10v": fmt.Sprintf("%10v", want), "%.40s": fmt.Sprintf("%.40s", want),
	} {
		if got := renders["Sprintf "+verb]; got != w {
			t.Errorf("Sprintf %s = %q, want Error()'s text %q", verb, got, w)
		}
	}
	if got := renders["Sprintf %d"]; got != "%!d(*exec.CommandError="+want+")" {
		t.Errorf("Sprintf %%d = %q, want fmt's bad-verb shape around Error()", got)
	}
	// %#v keeps the Go-syntax field list, redacted.
	gs := renders["Sprintf %#v"]
	for _, part := range []string{
		`&exec.CommandError{Name:"git", Args:[]string{"clone", "[redacted-arg]"}, Stderr:"[redacted]", StderrDropped:0, Output:"[redacted]", ExitCode:128, Err:*fmt.wrapError("[redacted]"), span:exec.opaqueSpan{`,
	} {
		if !strings.HasPrefix(gs, part) {
			t.Errorf("%%#v = %s\nwant the prefix %s", gs, part)
		}
	}
	// A directly nested *CommandError reads as its own GoString.
	outer := &CommandError{Name: "sh", Err: inner}
	if got := fmt.Sprintf("%#v", outer); leaks(got) || !strings.Contains(got, `Err:&exec.CommandError{Name:"git", Args:[]string{"fetch", "[redacted-arg]"}`) {
		t.Errorf("%%#v of a nested CommandError = %s", got)
	}

	var nilErr *CommandError
	for verb, w := range map[string]string{"%v": "<nil>", "%s": "<nil>", "%+v": "<nil>", "%#v": "(*exec.CommandError)(nil)"} {
		if got := fmt.Sprintf(verb, nilErr); got != w {
			t.Errorf("Sprintf(%s, nil *CommandError) = %q, want %q, as before Format existed", verb, got, w)
		}
	}
}
