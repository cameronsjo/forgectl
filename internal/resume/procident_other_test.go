//go:build !darwin

package resume

import "testing"

func TestStatStartTicks(t *testing.T) {
	// comm holds a space and a ')' — fields must be counted after the LAST ')'.
	stat := "4242 (my (odd) proc) S 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1234 56 18446744073709551615"
	got, err := statStartTicks(stat)
	if err != nil || got != 987654 {
		t.Fatalf("statStartTicks = %d, %v; want 987654", got, err)
	}
	for _, bad := range []string{"", "4242 no-comm S 1", "4242 (x) S 1 2 3"} {
		if _, err := statStartTicks(bad); err == nil {
			t.Errorf("statStartTicks(%q) accepted", bad)
		}
	}
}

func TestBootTime(t *testing.T) {
	got, err := bootTime("cpu 1 2 3\nbtime 1759190400\nprocesses 9\n")
	if err != nil || got.Unix() != 1759190400 {
		t.Fatalf("bootTime = %v, %v", got, err)
	}
	if _, err := bootTime("cpu 1 2 3\n"); err == nil {
		t.Error("no btime line: accepted")
	}
}
