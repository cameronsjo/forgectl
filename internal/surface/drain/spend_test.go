package drain

import (
	"reflect"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

func TestSumSpend(t *testing.T) {
	now := time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC)
	cost := func(v float64) *float64 { return &v }
	row := func(name, launch string, at time.Time, c *float64) worker.QueueRow {
		return worker.QueueRow{Name: name, LaunchID: launch, StateAt: at, CostUSD: c}
	}
	tests := []struct {
		name  string
		usage string
		rows  []worker.QueueRow
		days  int
		want  Spend
	}{
		{
			name: "nothing",
			days: 30,
			want: Spend{Days: 30, ByDay: []SpendDay{}},
		},
		{
			name:  "file lines and queue rows by day",
			usage: `{"day":"2026-10-08","name":"a","launch_id":"l1","costUsd":1.25}` + "\n",
			rows:  []worker.QueueRow{row("b", "l2", now, cost(2.22))},
			days:  30,
			want: Spend{Days: 30, TotalUSD: 3.47, ByDay: []SpendDay{
				{Day: "2026-10-08", USD: 1.25, Rows: 1},
				{Day: "2026-10-10", USD: 2.22, Rows: 1},
			}},
		},
		{
			name: "a duplicated file line counts once",
			usage: `{"day":"2026-10-09","name":"a","launch_id":"l1","costUsd":1}` + "\n" +
				`{"day":"2026-10-09","name":"a","launch_id":"l1","costUsd":1}` + "\n",
			days: 30,
			want: Spend{Days: 30, TotalUSD: 1, ByDay: []SpendDay{{Day: "2026-10-09", USD: 1, Rows: 1}}},
		},
		{
			name:  "a row in both places counts the file line",
			usage: `{"day":"2026-10-09","name":"a","launch_id":"l1","costUsd":1}` + "\n",
			rows:  []worker.QueueRow{row("a", "l1", now, cost(5))},
			days:  30,
			want:  Spend{Days: 30, TotalUSD: 1, ByDay: []SpendDay{{Day: "2026-10-09", USD: 1, Rows: 1}}},
		},
		{
			name:  "same name, new launch is a second row",
			usage: `{"day":"2026-10-09","name":"a","launch_id":"l1","costUsd":1}` + "\n",
			rows:  []worker.QueueRow{row("a", "l2", now, cost(2))},
			days:  30,
			want: Spend{Days: 30, TotalUSD: 3, ByDay: []SpendDay{
				{Day: "2026-10-09", USD: 1, Rows: 1},
				{Day: "2026-10-10", USD: 2, Rows: 1},
			}},
		},
		{
			name: "unparseable and invalid lines are skipped",
			usage: "not json\n" +
				`{"day":"2026-13-01","name":"a","launch_id":"l1","costUsd":1}` + "\n" +
				`{"day":"2026-10-09","name":"","launch_id":"l1","costUsd":1}` + "\n" +
				`{"day":"2026-10-09","name":"b","launch_id":"l1","costUsd":-1}` + "\n" +
				"\n" +
				`{"day":"2026-10-09","name":"c","launch_id":"l3","costUsd":0.5}`,
			days: 30,
			want: Spend{Days: 30, TotalUSD: 0.5, Skipped: 4, ByDay: []SpendDay{{Day: "2026-10-09", USD: 0.5, Rows: 1}}},
		},
		{
			name: "unpriced rows are not counted",
			rows: []worker.QueueRow{row("a", "l1", now, nil)},
			days: 30,
			want: Spend{Days: 30, ByDay: []SpendDay{}},
		},
		{
			name: "the window counts today and days-1 before it",
			usage: `{"day":"2026-10-08","name":"old","launch_id":"l0","costUsd":9}` + "\n" +
				`{"day":"2026-10-09","name":"in","launch_id":"l1","costUsd":1}` + "\n",
			rows: []worker.QueueRow{row("late", "l2", now.Add(-9*time.Hour), cost(2))},
			days: 2,
			want: Spend{Days: 2, TotalUSD: 3, ByDay: []SpendDay{
				{Day: "2026-10-09", USD: 1, Rows: 1},
				{Day: "2026-10-10", USD: 2, Rows: 1},
			}},
		},
		{
			name: "a queue row's day is its state_at in UTC",
			rows: []worker.QueueRow{row("a", "l1", time.Date(2026, 10, 9, 23, 30, 0, 0, time.FixedZone("x", -5*3600)), cost(1))},
			days: 30,
			want: Spend{Days: 30, TotalUSD: 1, ByDay: []SpendDay{{Day: "2026-10-10", USD: 1, Rows: 1}}},
		},
		{
			name: "sums round to the cent",
			usage: `{"day":"2026-10-09","name":"a","launch_id":"l1","costUsd":0.1}` + "\n" +
				`{"day":"2026-10-09","name":"b","launch_id":"l2","costUsd":0.2}` + "\n",
			days: 30,
			want: Spend{Days: 30, TotalUSD: 0.3, ByDay: []SpendDay{{Day: "2026-10-09", USD: 0.3, Rows: 2}}},
		},
		{
			name: "a cost past the ceiling is skipped, not summed to +Inf",
			usage: `{"day":"2026-10-09","name":"a","launch_id":"l1","costUsd":1e307}` + "\n" +
				`{"day":"2026-10-09","name":"b","launch_id":"l2","costUsd":1e307}` + "\n",
			rows: []worker.QueueRow{row("c", "l3", now, cost(1e307))},
			days: 30,
			want: Spend{Days: 30, Skipped: 3, ByDay: []SpendDay{}},
		},
		{
			name:  "lines and rows dated after today are skipped",
			usage: `{"day":"9999-12-31","name":"a","launch_id":"l1","costUsd":1}` + "\n",
			rows:  []worker.QueueRow{row("b", "l2", now.Add(48*time.Hour), cost(1))},
			days:  30,
			want:  Spend{Days: 30, Skipped: 2, ByDay: []SpendDay{}},
		},
		{
			name: "rows with no launch id are not merged",
			usage: `{"day":"2026-10-09","name":"a","launch_id":"","costUsd":1}` + "\n" +
				`{"day":"2026-10-09","name":"a","launch_id":"","costUsd":2}` + "\n",
			days: 30,
			want: Spend{Days: 30, TotalUSD: 3, ByDay: []SpendDay{{Day: "2026-10-09", USD: 3, Rows: 2}}},
		},
		{
			name: "the total rounds the raw sum",
			usage: `{"day":"2026-10-08","name":"a","launch_id":"l1","costUsd":0.004}` + "\n" +
				`{"day":"2026-10-09","name":"b","launch_id":"l2","costUsd":0.004}` + "\n" +
				`{"day":"2026-10-10","name":"c","launch_id":"l3","costUsd":0.004}` + "\n",
			days: 30,
			want: Spend{Days: 30, TotalUSD: 0.01, ByDay: []SpendDay{
				{Day: "2026-10-08", USD: 0, Rows: 1},
				{Day: "2026-10-09", USD: 0, Rows: 1},
				{Day: "2026-10-10", USD: 0, Rows: 1},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SumSpend([]byte(tt.usage), tt.rows, now, tt.days)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("SumSpend:\n got  %+v\n want %+v", got, tt.want)
			}
		})
	}
}
