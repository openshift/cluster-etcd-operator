package backuppolicycontroller

import (
	"time"

	"github.com/robfig/cron/v3"
)

// backupPolicyCronParser matches the parser used by BackupPolicyController.
var backupPolicyCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// scheduleIntervalGapsPerOrigin bounds Next() calls per sample origin so scrape
// stays cheap even for high-frequency cron (e.g. every minute).
const scheduleIntervalGapsPerOrigin = 64

// scheduleIntervalSampleOrigins are fixed UTC points chosen to expose irregular
// gaps (weekends, month/year boundaries) without walking multi-year timelines.
var scheduleIntervalSampleOrigins = []time.Time{
	time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),   // Wednesday
	time.Date(2020, 1, 3, 12, 0, 0, 0, time.UTC),  // Friday → weekend gap
	time.Date(2020, 1, 31, 0, 0, 0, 0, time.UTC),  // month end
	time.Date(2020, 2, 28, 0, 0, 0, 0, time.UTC),  // February end
	time.Date(2020, 12, 31, 0, 0, 0, 0, time.UTC), // year end
}

// estimateScheduleIntervalSeconds returns the maximum gap between consecutive
// firings of schedule, sampled from fixed origins. Invalid schedules return
// ok=false so callers can omit the interval metric (NoRecentSuccess then cannot
// match on it).
func estimateScheduleIntervalSeconds(schedule string) (seconds float64, ok bool) {
	sched, err := backupPolicyCronParser.Parse(schedule)
	if err != nil {
		return 0, false
	}

	if constant, ok := sched.(cron.ConstantDelaySchedule); ok {
		if constant.Delay <= 0 {
			return 0, false
		}
		return constant.Delay.Seconds(), true
	}

	var maxGap time.Duration
	for _, origin := range scheduleIntervalSampleOrigins {
		prev := sched.Next(origin)
		if prev.IsZero() || !prev.After(origin) {
			continue
		}
		t := prev
		for i := 0; i < scheduleIntervalGapsPerOrigin; i++ {
			next := sched.Next(t)
			if next.IsZero() || !next.After(t) {
				break
			}
			gap := next.Sub(t)
			if gap > maxGap {
				maxGap = gap
			}
			t = next
		}
	}
	if maxGap <= 0 {
		return 0, false
	}
	return maxGap.Seconds(), true
}
