package handlers

import (
	"testing"
	"time"
)

// exportClock is 09:49 UTC on 2026-11-06, 10:49 in Berlin (CET): the export
// tests expect "-2026-11-06-1049" in every name.
var exportClock = time.Date(2026, 11, 6, 9, 49, 0, 0, time.UTC)

// An export's name carries the time it was made, on the Berlin calendar, so
// an older and a newer download differ (system_3 #9990, from #9964).
func TestExportFilename_BerlinTime(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"winter, CET", exportClock, "Rechnungen-2026-11-06-1049.csv"},
		// 23:30 UTC is already the next day in Berlin.
		{"after midnight Berlin, CET", time.Date(2026, 11, 6, 23, 30, 0, 0, time.UTC), "Rechnungen-2026-11-07-0030.csv"},
		// Summer time is two hours ahead.
		{"after midnight Berlin, CEST", time.Date(2026, 7, 14, 22, 5, 0, 0, time.UTC), "Rechnungen-2026-07-15-0005.csv"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withInvoicingClock(t, tc.at)
			if got := exportFilename("Rechnungen", "csv"); got != tc.want {
				t.Errorf("exportFilename = %q, want %q", got, tc.want)
			}
		})
	}
}
