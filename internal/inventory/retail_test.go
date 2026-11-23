package inventory

import "testing"

func TestMoveAndStatsText(t *testing.T) {
	if got := (StockMove{Delta: 2}).DeltaText(); got != "+2" {
		t.Fatalf("入库应显示 +2，得到 %q", got)
	}
	if got := (StockMove{Delta: -1}).DeltaText(); got != "-1" {
		t.Fatalf("售出应显示 -1，得到 %q", got)
	}

	st := RetailStats{TodayRev: 6000, TodayCost: 3500, MonthRev: 18600, MonthCost: 10900}
	if st.TodayProfitText() != "¥ 25" || st.MonthProfitText() != "¥ 77" {
		t.Fatalf("毛利文案不对: %s / %s", st.TodayProfitText(), st.MonthProfitText())
	}
	if (RetailStats{TodayRev: 6000}).TodayRevText() != "¥ 60" {
		t.Fatalf("营收文案不对: %q", RetailStats{TodayRev: 6000}.TodayRevText())
	}
}
