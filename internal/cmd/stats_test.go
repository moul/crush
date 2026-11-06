package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMergeStatsKeepsDayOrder checks that merging keeps the day order the
// queries give: the daily table takes its first 30 rows as the latest 30
// days, and the recent activity chart draws its days left to right.
func TestMergeStatsKeepsDayOrder(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var latestFirst, oldestFirst []string
	stats := &Stats{Total: TotalStats{TotalSessions: 40}}
	for i := range 40 {
		day := start.AddDate(0, 0, i).Format(time.DateOnly)
		oldestFirst = append(oldestFirst, day)
		latestFirst = append([]string{day}, latestFirst...)
		// GetUsageByDay orders by day DESC, GetRecentActivity by day ASC.
		stats.UsageByDay = append([]DailyUsage{{Day: day, SessionCount: 1}}, stats.UsageByDay...)
		stats.RecentActivity = append(stats.RecentActivity, DailyActivity{Day: day, SessionCount: 1})
	}

	merged := mergeStats([]ProjectStats{{Stats: stats}})

	var byDay, recent []string
	for _, d := range merged.UsageByDay {
		byDay = append(byDay, d.Day)
	}
	for _, d := range merged.RecentActivity {
		recent = append(recent, d.Day)
	}
	require.Equal(t, latestFirst, byDay, "usage by day, latest first")
	require.Equal(t, oldestFirst, recent, "recent activity, oldest first")
}
