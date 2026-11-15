// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package herdr

import "testing"

func TestDetectHost(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Host
	}{
		{"neither", map[string]string{}, HostNone},
		{"only HERDR_ENV", map[string]string{"HERDR_ENV": "1"}, HostHerdr},
		{"only HERDR_SOCKET_PATH", map[string]string{"HERDR_SOCKET_PATH": "/s"}, HostHerdr},
		{"only HERDR_PANE_ID", map[string]string{"HERDR_PANE_ID": "p1"}, HostHerdr},
		{"wrong HERDR_ENV still herdr", map[string]string{"HERDR_ENV": "0"}, HostHerdr},
		{"only HC_TERMINAL_ID", map[string]string{"HC_TERMINAL_ID": "t4s"}, HostApp},
		{"both: herdr wins", map[string]string{"HERDR_ENV": "1", "HC_TERMINAL_ID": "t4s"}, HostHerdr},
		{"empty HC_TERMINAL_ID", map[string]string{"HC_TERMINAL_ID": ""}, HostNone},
		{"blank HC_TERMINAL_ID", map[string]string{"HC_TERMINAL_ID": "  "}, HostNone},
		{"unrelated vars", map[string]string{"HOME": "/h", "TERM": "xterm"}, HostNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectHost(func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok })
			if got != tc.want {
				t.Errorf("DetectHost = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHostString(t *testing.T) {
	for h, want := range map[Host]string{HostNone: "none", HostHerdr: "herdr", HostApp: "herdr-app"} {
		if got := h.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", h, got, want)
		}
	}
}
