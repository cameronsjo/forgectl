package termsafe

import (
	"strings"
	"testing"
)

func TestKillOthersPrompt_NamesTheTargets(t *testing.T) {
	cases := []struct {
		doomed []string
		want   string
	}{
		{[]string{"other", "t"}, `Keep "victim" and kill the other 2 sessions ("other", "t")?`},
		{[]string{"solo"}, `Keep "victim" and kill the other 1 session ("solo")?`},
		{[]string{"a", "b", "c", "d", "e", "f", "g", "h"}, `Keep "victim" and kill the other 8 sessions ("a", "b", "c", "d", "e", "f", and 2 more)?`},
		{[]string{"bad\x1b[2Jname"}, `Keep "victim" and kill the other 1 session ("bad\x1b[2Jname")?`},
		{[]string{strings.Repeat("x", 50)}, `Keep "victim" and kill the other 1 session ("` + strings.Repeat("x", 40) + `"…)?`},
	}
	for _, c := range cases {
		if got := KillOthersPrompt("victim", c.doomed); got != c.want {
			t.Errorf("KillOthersPrompt(%q) =\n %s\nwant\n %s", c.doomed, got, c.want)
		}
	}
}
