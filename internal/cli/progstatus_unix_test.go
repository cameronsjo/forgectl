// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"os"
	"syscall"
	"testing"
)

func TestCatchableSignals_SkipsIgnored(t *testing.T) {
	cases := []struct {
		name    string
		ignored map[os.Signal]bool
		want    []os.Signal
	}{
		{"none ignored", nil, []os.Signal{syscall.SIGINT, syscall.SIGTERM}},
		{"SIGINT ignored (background job in a POSIX shell)", map[os.Signal]bool{syscall.SIGINT: true}, []os.Signal{syscall.SIGTERM}},
		{"SIGTERM ignored", map[os.Signal]bool{syscall.SIGTERM: true}, []os.Signal{syscall.SIGINT}},
		{"both ignored", map[os.Signal]bool{syscall.SIGINT: true, syscall.SIGTERM: true}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := catchableSignals(func(s os.Signal) bool { return tc.ignored[s] })
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
