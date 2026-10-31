//go:build unix

// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/desk/ask"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// deskAskStdin is where `desk ask --steps-file -` reads; tests swap it.
var deskAskStdin io.Reader = os.Stdin

// maxStepsRead caps a steps file read, one byte past the limit so an
// oversized file is refused rather than cut.
const maxStepsRead = ask.MaxSteps + 1

func runDeskAsk(cmd *cobra.Command, _ module.Deps, dirFlag string, o deskAskOpts) error {
	if strings.TrimSpace(o.title) == "" {
		return deskUsage("desk ask: --title is required")
	}
	bucket, err := ask.ParseBucket(o.bucket)
	if err != nil {
		return deskUsage("desk ask: --bucket: %w", err)
	}
	allow, err := ask.ParseKinds(o.allow)
	if err != nil {
		return deskUsage("desk ask: --allow: %w", err)
	}
	links, err := parseAskLinks(o.links)
	if err != nil {
		return deskUsage("desk ask: %w", err)
	}
	steps, err := readSteps(o.stepsFile)
	if err != nil {
		return deskUsage("desk ask: --steps-file: %w", err)
	}
	notify := o.notify
	if notify == "self" {
		notify = deskGetenv("HC_TERMINAL_ID")
		if notify == "" {
			return deskUsage("desk ask: --notify self needs $HC_TERMINAL_ID (run inside an hc terminal, or pass the address)")
		}
	}
	from := o.from
	if from == "" {
		from = defaultAskFrom(deskGetenv)
	}
	s, _, err := openAskStore(dirFlag)
	if err != nil {
		return err
	}
	a, err := s.Add(ask.Ask{Title: o.title, Steps: steps, Bucket: bucket, Links: links, From: from, Notify: notify, Allow: allow})
	if errors.Is(err, ask.ErrInvalid) {
		return deskUsage("desk ask: %w", err)
	}
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if o.asJSON {
		return termsafe.JSONEncoder(out).Encode(a)
	}
	w := &stickyWriter{w: out}
	w.printf("id=%s\nbucket=%s\nwatch=forgectl desk watch --responses %s --deadline %d\n", a.ID, a.Bucket, a.ID, deskWatchDefaultDeadline)
	return w.err
}

// defaultAskFrom names the asker from the environment: a launched worker's
// name, else the Claude session.
func defaultAskFrom(getenv func(string) string) string {
	if w := getenv("FORGECTL_WORKER"); w != "" {
		return w
	}
	if sid := getenv("CLAUDE_CODE_SESSION_ID"); len(sid) >= 8 {
		return "session " + sid[:8]
	}
	return ""
}

// parseAskLinks reads --link values: URL, or LABEL=URL split at the "=" that
// starts an http(s) URL.
func parseAskLinks(raw []string) ([]ask.Link, error) {
	links := make([]ask.Link, 0, len(raw))
	for _, v := range raw {
		l := ask.Link{URL: strings.TrimSpace(v)}
		for _, scheme := range []string{"=https://", "=http://"} {
			if i := strings.Index(v, scheme); i >= 0 {
				l = ask.Link{Label: strings.TrimSpace(v[:i]), URL: strings.TrimSpace(v[i+1:])}
				break
			}
		}
		if !ask.SafeURL(l.URL) {
			return nil, fmt.Errorf("--link %q is not an http or https URL (URL or LABEL=URL)", termsafe.SafeLineMax(v, deskQuoteMax))
		}
		links = append(links, l)
	}
	return links, nil
}

// readSteps reads the steps file, or stdin for "-".
func readSteps(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	var r io.Reader
	if path == "-" {
		r = deskAskStdin
	} else {
		f, err := os.Open(path) //nolint:gosec // G304: the caller names its own steps file
		if err != nil {
			return "", termsafe.Error(err)
		}
		defer f.Close() //nolint:errcheck // read-only
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxStepsRead))
	if err != nil {
		return "", err
	}
	if len(data) > ask.MaxSteps {
		return "", fmt.Errorf("the steps are longer than %d bytes", ask.MaxSteps)
	}
	return string(data), nil
}

func runDeskAsks(cmd *cobra.Command, _ module.Deps, dirFlag, id string, all, asJSON bool) error {
	dir, err := resolveDeskDir(dirFlag)
	if err != nil {
		return err
	}
	s, err := ask.OpenReadOnly(dir, ask.WithClock(deskNow))
	if err != nil {
		return err
	}
	items, err := s.Items()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if id != "" {
		it, ok := ask.Find(items, id)
		if !ok {
			return fmt.Errorf("desk asks: no ask %s", termsafe.SafeLineMax(id, deskQuoteMax))
		}
		if asJSON {
			return termsafe.JSONEncoder(out).Encode(it)
		}
		return writeAskDetail(out, it)
	}
	if !all {
		kept := items[:0]
		for _, it := range items {
			if it.State != ask.StateAnswered {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	if asJSON {
		if items == nil {
			items = []ask.Item{}
		}
		return termsafe.JSONEncoder(out).Encode(items)
	}
	return writeAskList(out, items, deskNow())
}

// writeAskList prints asks grouped by bucket, then snoozed, then answered.
func writeAskList(out io.Writer, items []ask.Item, now time.Time) error {
	w := &stickyWriter{w: out}
	open, snoozed, answered := ask.Grouped(items)
	shown := false
	for _, b := range ask.Buckets {
		if len(open[b]) == 0 {
			continue
		}
		shown = true
		w.printf("%s\n", b.Label())
		for _, it := range open[b] {
			w.printf("  %s  %s%s\n", it.Ask.ID, safeText(it.Ask.Title), askFromSuffix(it.Ask, now))
		}
	}
	if len(snoozed) > 0 {
		shown = true
		w.printf("Snoozed\n")
		for _, it := range snoozed {
			w.printf("  %s  %s  (until %s)\n", it.Ask.ID, safeText(it.Ask.Title), it.SnoozedUntil.Local().Format("Mon 15:04"))
		}
	}
	if len(answered) > 0 {
		shown = true
		w.printf("Answered\n")
		for _, it := range answered {
			w.printf("  %s  %s  (%s)\n", it.Ask.ID, safeText(it.Ask.Title), it.Answer.Kind)
		}
	}
	if !shown {
		w.printf("no open asks\n")
	}
	return w.err
}

func askFromSuffix(a ask.Ask, now time.Time) string {
	age := now.Sub(a.At).Round(time.Minute)
	if a.From == "" {
		return fmt.Sprintf("  (%s ago)", age)
	}
	return fmt.Sprintf("  (%s, %s ago)", safeText(a.From), age)
}

// writeAskDetail prints one ask in full.
func writeAskDetail(out io.Writer, it ask.Item) error {
	w := &stickyWriter{w: out}
	a := it.Ask
	w.printf("id=%s\nstate=%s\nbucket=%s\ntitle=%s\n", a.ID, it.State, a.Bucket, safeText(a.Title))
	if a.From != "" {
		w.printf("from=%s\n", safeText(a.From))
	}
	if a.Notify != "" {
		w.printf("notify=%s\n", a.Notify)
	}
	kinds := make([]string, len(a.Allow))
	for i, k := range a.Allow {
		kinds[i] = string(k)
	}
	w.printf("allow=%s\n", strings.Join(kinds, ","))
	for _, l := range a.Links {
		w.printf("link=%s %s\n", safeText(l.URL), safeText(l.Label))
	}
	if a.Steps != "" {
		w.printf("steps:\n")
		for _, line := range strings.Split(a.Steps, "\n") {
			w.printf("  %s\n", safeText(line))
		}
	}
	for _, r := range it.Responses {
		w.printf("response=%s %s %s%s\n", r.ID, r.At.UTC().Format(time.RFC3339), r.Kind, responseDetail(r))
	}
	for _, n := range it.Notified {
		status := "ok"
		if !n.OK {
			status = "failed: " + safeText(n.Error)
		}
		w.printf("notified=%s %s %s\n", n.Response, n.Address, status)
	}
	return w.err
}

func responseDetail(r ask.Response) string {
	switch r.Kind {
	case ask.KindReply:
		return " " + safeText(strings.Join(strings.Fields(r.Text), " "))
	case ask.KindLink:
		return " " + safeText(r.URL)
	case ask.KindSnooze:
		if r.Until != nil {
			return " until " + r.Until.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

func runDeskRespond(cmd *cobra.Command, _ module.Deps, dirFlag string, args []string, asJSON bool) error {
	id, kind := args[0], ask.Kind(strings.ToLower(args[1]))
	r := ask.Response{Ask: id, Kind: kind, Via: "cli"}
	value := ""
	if len(args) == 3 {
		value = args[2]
	}
	switch kind {
	case ask.KindDone:
		if value != "" {
			return deskUsage("desk respond: done takes no value")
		}
	case ask.KindReply:
		r.Text = value
	case ask.KindLink:
		r.URL = value
	case ask.KindSnooze:
		d, err := parseSnooze(value)
		if err != nil {
			return deskUsage("desk respond: %w", err)
		}
		until := deskNow().Add(d)
		r.Until = &until
	default:
		return deskUsage("desk respond: %q is not done, reply, link or snooze", termsafe.SafeLineMax(args[1], deskQuoteMax))
	}
	if kind != ask.KindDone && value == "" {
		return deskUsage("desk respond: %s needs a value", kind)
	}
	s, _, err := openAskStore(dirFlag)
	if err != nil {
		return err
	}
	it, saved, notifyErr, err := s.Respond(cmd.Context(), r)
	switch {
	case errors.Is(err, ask.ErrInvalid), errors.Is(err, ask.ErrNotAllowed):
		return deskUsage("desk respond: %w", err)
	case err != nil:
		return fmt.Errorf("desk respond: %w", err)
	}
	if asJSON {
		v := struct {
			Response    ask.Response `json:"response"`
			Item        ask.Item     `json:"item"`
			NotifyError string       `json:"notify_error,omitempty"`
		}{Response: saved, Item: it}
		if notifyErr != nil {
			v.NotifyError = notifyErr.Error()
		}
		return termsafe.JSONEncoder(cmd.OutOrStdout()).Encode(v)
	}
	w := &stickyWriter{w: cmd.OutOrStdout()}
	w.printf("response=%s\nstate=%s\n", saved.ID, it.State)
	if it.Ask.Notify != "" {
		if notifyErr != nil {
			w.printf("notified=failed %s\n", safeText(notifyErr.Error()))
		} else {
			w.printf("notified=%s\n", it.Ask.Notify)
		}
	}
	return w.err
}

// parseSnooze reads a Go duration or Nd days.
func parseSnooze(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("snooze %q is not a duration (90m, 4h, 2d)", termsafe.SafeLineMax(s, deskQuoteMax))
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("snooze %q is not a duration (90m, 4h, 2d)", termsafe.SafeLineMax(s, deskQuoteMax))
	}
	return d, nil
}

// askWatchLine is one JSON line a --responses watch prints.
type askWatchLine struct {
	Ask      string       `json:"ask"`
	Title    string       `json:"title"`
	Response ask.Response `json:"response"`
	// Offset is the log offset just past this response: --since resumes
	// after it.
	Offset int64     `json:"offset"`
	State  ask.State `json:"state"`
}

func runDeskWatchResponses(cmd *cobra.Command, _ module.Deps, dirFlag, id string, deadline int, since int64) error {
	if id != "" && !ask.ValidID(id) {
		return deskUsage("desk watch: %q is not an ask id (ask-xxxxxxxx)", termsafe.SafeLineMax(id, deskQuoteMax))
	}
	dir, err := resolveDeskDir(dirFlag)
	if err != nil {
		return err
	}
	if dirFlag != "" {
		if _, err := shellQuote(dir); err != nil {
			return deskUsage("desk watch: --dir %w, so a resume= line could not name it", err)
		}
	}
	s, err := ask.OpenReadOnly(dir, ask.WithClock(deskNow))
	if err != nil {
		return err
	}
	if deadline == deskWatchDeadlineUnset {
		deadline = 0
		if !deskStdinIsTerminal() {
			deadline = deskWatchDefaultDeadline
		}
	}
	signal.Ignore(syscall.SIGPIPE)
	defer signal.Reset(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	watchCtx := ctx
	if deadline > 0 {
		var cancel context.CancelFunc
		watchCtx, cancel = context.WithTimeout(ctx, time.Duration(deadline)*time.Second)
		defer cancel()
	}
	w := &stickyWriter{w: cmd.OutOrStdout()}

	// An ask watch reads the whole log first: the ask must exist, and an
	// answer given before the watch started still ends it.
	off := since
	if id != "" {
		off = 0
	}
	asks := map[string]ask.Ask{}
	var recs []ask.Record
	for {
		batch, next, _, err := s.ReadFrom(off)
		if err != nil {
			return err
		}
		recs = append(recs, batch...)
		for _, rec := range batch {
			if rec.Ask != nil {
				asks[rec.Ask.ID] = *rec.Ask
			}
		}
		if _, ok := asks[id]; id != "" && !ok && len(recs) == len(batch) {
			// The first read covers the whole log; the ask must be in it.
			return fmt.Errorf("desk watch: no ask %s", id)
		}
		items := ask.Fold(recs, deskNow())
		for _, rec := range batch {
			r := rec.Response
			if r == nil || (id != "" && r.Ask != id) || (id != "" && rec.Offset <= since) {
				continue
			}
			line := askWatchLine{Ask: r.Ask, Title: asks[r.Ask].Title, Response: *r, Offset: rec.Offset}
			if it, ok := ask.Find(items, r.Ask); ok {
				line.State = it.State
			}
			var buf bytes.Buffer
			if err := termsafe.JSONEncoder(&buf).Encode(line); err != nil {
				return err
			}
			w.printf("%s", buf.String())
			if w.err != nil {
				return WithExitCode(fmt.Errorf("desk watch: stdout closed; resume with %s", responsesResume(id, rec.Offset, deadline, dirFlag)), deskExitBrokenPipe)
			}
		}
		off = next
		if id != "" {
			if it, ok := ask.Find(items, id); ok && it.State == ask.StateAnswered {
				return nil
			}
		}
		select {
		case <-watchCtx.Done():
			w.printf("resume=%s\n", responsesResume(id, off, deadline, dirFlag))
			if ctx.Err() != nil {
				return WithExitCode(errors.New("desk watch: interrupted"), deskExitInterrupted)
			}
			what := "a response"
			if id != "" {
				what = id + " to be answered"
			}
			return WithExitCode(fmt.Errorf("desk watch: waited %ds for %s", deadline, what), deskExitTempFail)
		case <-time.After(deskWatchInterval):
		}
	}
}

// responsesResume is the command that continues a --responses watch.
func responsesResume(id string, off int64, deadline int, dirFlag string) string {
	s := "forgectl desk watch --responses"
	if id != "" {
		s += " " + id
	}
	s += " --since " + strconv.FormatInt(off, 10)
	if deadline > 0 {
		s += " --deadline " + strconv.Itoa(deadline)
	}
	if dirFlag != "" {
		if dir, err := resolveDeskDir(dirFlag); err == nil {
			if q, err := shellQuote(dir); err == nil {
				s += " --dir " + q
			}
		}
	}
	return s
}
