package report

import (
	"testing"
	"time"

	"pourops/internal/platform"
)

func TestReportDates(t *testing.T) {
	ts := func(year, month, day, hour, min, sec int) time.Time {
		return time.Date(year, time.Month(month), day, hour, min, sec, 0, platform.ReportZone)
	}

	// 2024-03-01 00:00:00 北京时间仍属于 2024-02-29 营业日（07:00 才切到 03-01）
	beforeCutoff := time.Date(2024, 2, 29, 16, 0, 0, 0, time.UTC)
	// 2024-03-01 07:00:00 北京时间切入 2024-03-01 营业日
	atCutoff := ts(2024, 3, 1, 7, 0, 0)

	for _, tc := range []struct {
		name                           string
		now                            time.Time
		period, from, to               string
		wantPeriod, wantStart, wantEnd string
		wantStartAt, wantEndAt         time.Time
	}{
		{
			name: "零点前未过7点默认7天仍归2月29",
			now:  beforeCutoff, wantPeriod: "7d", wantStart: "2024-02-23", wantEnd: "2024-03-01",
			wantStartAt: ts(2024, 2, 23, 7, 0, 0), wantEndAt: ts(2024, 3, 1, 7, 0, 0),
		},
		{
			name: "7点后默认7天归3月1",
			now:  atCutoff, period: "", wantPeriod: "7d", wantStart: "2024-02-24", wantEnd: "2024-03-02",
			wantStartAt: ts(2024, 2, 24, 7, 0, 0), wantEndAt: ts(2024, 3, 2, 7, 0, 0),
		},
		{
			name: "06:59今日仍属2月29",
			now:  ts(2024, 3, 1, 6, 59, 59), period: "today", wantPeriod: "today", wantStart: "2024-02-29", wantEnd: "2024-03-01",
			wantStartAt: ts(2024, 2, 29, 7, 0, 0), wantEndAt: ts(2024, 3, 1, 7, 0, 0),
		},
		{
			name: "07:00今日切到3月1",
			now:  atCutoff, period: "today", wantPeriod: "today", wantStart: "2024-03-01", wantEnd: "2024-03-02",
			wantStartAt: ts(2024, 3, 1, 7, 0, 0), wantEndAt: ts(2024, 3, 2, 7, 0, 0),
		},
		{
			name: "06:59本月仍是2月整月",
			now:  beforeCutoff, period: "month", wantPeriod: "month", wantStart: "2024-02-01", wantEnd: "2024-03-01",
			wantStartAt: ts(2024, 2, 1, 7, 0, 0), wantEndAt: ts(2024, 3, 1, 7, 0, 0),
		},
		{
			name: "07:00本月切到3月1",
			now:  atCutoff, period: "month", wantPeriod: "month", wantStart: "2024-03-01", wantEnd: "2024-03-02",
			wantStartAt: ts(2024, 3, 1, 7, 0, 0), wantEndAt: ts(2024, 3, 2, 7, 0, 0),
		},
		{
			name: "30天窗口",
			now:  atCutoff, period: "30d", wantPeriod: "30d", wantStart: "2024-02-01", wantEnd: "2024-03-02",
			wantStartAt: ts(2024, 2, 1, 7, 0, 0), wantEndAt: ts(2024, 3, 2, 7, 0, 0),
		},
		{
			name: "自定义单日1月11实际是11日07点到12日07点",
			now:  atCutoff, period: "custom", from: "2026-01-11", to: "2026-01-11",
			wantPeriod: "custom", wantStart: "2026-01-11", wantEnd: "2026-01-12",
			wantStartAt: ts(2026, 1, 11, 7, 0, 0), wantEndAt: ts(2026, 1, 12, 7, 0, 0),
		},
		{
			name: "自定义366天上限",
			now:  atCutoff, period: "custom", from: "2024-01-01", to: "2024-12-31",
			wantPeriod: "custom", wantStart: "2024-01-01", wantEnd: "2025-01-01",
			wantStartAt: ts(2024, 1, 1, 7, 0, 0), wantEndAt: ts(2025, 1, 1, 7, 0, 0),
		},
		{name: "超366天拒绝", now: atCutoff, period: "custom", from: "2024-01-01", to: "2025-01-01"},
		{name: "非法日期拒绝", now: atCutoff, period: "custom", from: "2024-02-30", to: "2024-03-02"},
		{name: "逆序拒绝", now: atCutoff, period: "custom", from: "2024-03-02", to: "2024-03-01"},
		{name: "缺结束日拒绝", now: atCutoff, period: "custom", from: "2024-03-01", to: ""},
		{name: "未知周期拒绝", now: atCutoff, period: "year"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			period, start, end, problem := ReportDates(tc.now, tc.period, tc.from, tc.to)
			if tc.wantPeriod == "" {
				if problem == "" {
					t.Fatalf("错误范围未拒绝: %+v", tc)
				}
				return
			}
			if problem != "" || period != tc.wantPeriod || start.Format(time.DateOnly) != tc.wantStart || end.Format(time.DateOnly) != tc.wantEnd {
				t.Fatalf("范围得到 %q %v %v (%s)", period, start, end, problem)
			}
			if !start.Equal(tc.wantStartAt) || !end.Equal(tc.wantEndAt) {
				t.Fatalf("边界时刻 = [%v, %v), want [%v, %v)", start, end, tc.wantStartAt, tc.wantEndAt)
			}
		})
	}
}
