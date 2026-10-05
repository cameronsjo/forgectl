package desk

import (
	"reflect"
	"testing"
	"time"
)

func TestParseStatusTSV(t *testing.T) {
	data := []byte("step\tstate\trc\tstart\tend\tdeps\n" +
		"fetch\tok\t0\t1700000000.250\t1700000001.500\t-\n" +
		"build\trunning\t-\t1700000002.000\t-\tfetch\n" +
		"pack\tpending\t-\t-\t-\tbuild,BAD_ID\n" +
		"BAD\tok\t0\t-\t-\t-\n" + // not a step id: dropped
		"short\tok\n" + // half-written row: dropped
		"\n")
	rc0 := 0
	want := []StepStatus{
		{ID: "fetch", State: StepOK, RC: &rc0, Start: time.UnixMilli(1700000000250).UTC(), End: time.UnixMilli(1700000001500).UTC()},
		{ID: "build", State: StepRunning, Start: time.UnixMilli(1700000002000).UTC(), Deps: []string{"fetch"}},
		{ID: "pack", State: StepPending, Deps: []string{"build"}},
	}
	if got := ParseStatusTSV(data); !reflect.DeepEqual(got, want) {
		t.Errorf("ParseStatusTSV =\n%+v\nwant\n%+v", got, want)
	}
	if got := ParseStatusTSV(nil); got != nil {
		t.Errorf("empty input = %+v", got)
	}
}
