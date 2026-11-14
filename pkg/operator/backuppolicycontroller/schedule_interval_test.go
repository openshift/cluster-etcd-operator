package backuppolicycontroller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEstimateScheduleIntervalSeconds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		schedule string
		want     time.Duration
		wantOK   bool
	}{
		{name: "hourly-macro", schedule: "@hourly", want: time.Hour, wantOK: true},
		{name: "daily-macro", schedule: "@daily", want: 24 * time.Hour, wantOK: true},
		{name: "weekly-macro", schedule: "@weekly", want: 7 * 24 * time.Hour, wantOK: true},
		{name: "every-2h", schedule: "@every 2h", want: 2 * time.Hour, wantOK: true},
		{name: "daily-cron", schedule: "0 1 * * *", want: 24 * time.Hour, wantOK: true},
		{name: "invalid", schedule: "not a schedule", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := estimateScheduleIntervalSeconds(tc.schedule)
			require.Equal(t, tc.wantOK, ok)
			if !tc.wantOK {
				return
			}
			require.InDelta(t, tc.want.Seconds(), got, 1)
		})
	}
}

func TestEstimateScheduleIntervalSecondsWeekdays(t *testing.T) {
	t.Parallel()
	// Mon-Fri at midnight: largest gap is Friday→Monday (3 days).
	got, ok := estimateScheduleIntervalSeconds("0 0 * * 1-5")
	require.True(t, ok)
	require.InDelta(t, (3 * 24 * time.Hour).Seconds(), got, 1)
}
