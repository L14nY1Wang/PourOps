package report

import (
	"testing"
	"time"
)

func TestReportDates(t *testing.T) {
	now := time.Date(2024, 2, 29, 16, 0, 0, 0, time.UTC) // 北京时间 3 月 1 日零点
	for _, tc := range []struct{ period, from, to, wantPeriod, wantStart, wantEnd string }{
		{"", "", "", "7d", "2024-02-24", "2024-03-02"},
		{"today", "", "", "today", "2024-03-01", "2024-03-02"},
		{"month", "", "", "month", "2024-03-01", "2024-03-02"},
		{"30d", "", "", "30d", "2024-02-01", "2024-03-02"},
		{"custom", "2024-02-29", "2024-02-29", "custom", "2024-02-29", "2024-03-01"},
		{"custom", "2024-01-01", "2024-12-31", "custom", "2024-01-01", "2025-01-01"},
		{"custom", "2024-01-01", "2025-01-01", "", "", ""},
		{"custom", "2024-02-30", "2024-03-02", "", "", ""},
		{"custom", "2024-03-02", "2024-03-01", "", "", ""},
		{"custom", "2024-03-01", "", "", "", ""},
		{"year", "", "", "", "", ""},
	} {
		period, start, end, problem := ReportDates(now, tc.period, tc.from, tc.to)
		if tc.wantPeriod == "" {
			if problem == "" {
				t.Errorf("错误范围未拒绝: %+v", tc)
			}
			continue
		}
		if problem != "" || period != tc.wantPeriod || start.Format(time.DateOnly) != tc.wantStart || end.Format(time.DateOnly) != tc.wantEnd {
			t.Errorf("范围 %+v 得到 %q %v %v (%s)", tc, period, start, end, problem)
		}
	}
}
