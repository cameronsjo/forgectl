package exec

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"testing/iotest"
)

// TestReadTail keeps the end of a stream longer than its cap, says the start
// was dropped, and leaves a short stream whole.
func TestReadTail(t *testing.T) {
	const limit = 1000
	long := strings.Repeat("a", 100_000) + "END"
	out := make(chan BoundedOutput, 1)
	readTail(iotest.OneByteReader(strings.NewReader(long[:5000])), limit, out)
	got := <-out
	data, complete := got.CopyBytesForParse()
	if complete || !got.HeadDropped() || len(data) != limit || string(data) != long[4000:5000] {
		t.Fatalf("one-byte reads: complete=%v dropped=%v len=%d", complete, got.HeadDropped(), len(data))
	}

	readTail(strings.NewReader(long), limit, out)
	got = <-out
	data, complete = got.CopyBytesForParse()
	if complete || !got.HeadDropped() || len(data) != limit || !bytes.HasSuffix(data, []byte("END")) {
		t.Fatalf("long stream: complete=%v dropped=%v len=%d suffix=%q", complete, got.HeadDropped(), len(data), data[len(data)-3:])
	}

	readTail(strings.NewReader("short"), limit, out)
	got = <-out
	if data, complete := got.CopyBytesForParse(); !complete || got.HeadDropped() || string(data) != "short" {
		t.Fatalf("short stream: %q complete=%v dropped=%v", data, complete, got.HeadDropped())
	}

	readTail(iotest.ErrReader(context.Canceled), limit, out)
	if got := <-out; got.Complete() {
		t.Fatal("a read that failed before EOF reported itself complete")
	}
}

// TestRunSensitive_TailCaptureKeepsTheEnd: a CaptureTail stream past its cap
// is not an output-limit failure. The process runs to its end, and the
// retained bytes are its last cap bytes.
func TestRunSensitive_TailCaptureKeepsTheEnd(t *testing.T) {
	const limit = 8192
	runner, self := helperRunner(t, "floodend:400000", defaultRetireBound)
	cmd := helperCommand(KindHcRead, self, limit)
	cmd.StdoutMode = CaptureTail
	res, _, err := runSensitiveWithin(context.Background(), t, runner, cmd)
	if err != nil {
		t.Fatalf("err = %v, want none: a long tail read is not a failure", err)
	}
	data, complete := res.Stdout.CopyBytesForParse()
	if complete || !res.Stdout.HeadDropped() {
		t.Errorf("complete=%v dropped=%v, want an incomplete read with its start dropped", complete, res.Stdout.HeadDropped())
	}
	if len(data) != limit || !bytes.HasSuffix(data, []byte("END-OF-SCREEN")) {
		t.Errorf("kept %d bytes ending %q, want the last %d bytes", len(data), data[max(0, len(data)-13):], limit)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0: the producer must run to its end", res.ExitCode)
	}
}
