package inventory

import (
	"testing"
	"time"

	"pourops/internal/platform"
)

func TestKegMoney(t *testing.T) {
	finished := time.Date(2026, 11, 15, 20, 0, 0, 0, time.Local)
	k := Keg{CapacityML: 20000, CostCents: 128000, SoldCups: 3, SoldML: 1330,
		RevenueCents: 18100, FinishedAt: &finished}

	if got, want := k.RemainingML(), 18670; got != want {
		t.Errorf("RemainingML = %d, want %d", got, want)
	}
	if got, want := k.SoldPct(), 6; got != want {
		t.Errorf("SoldPct = %d, want %d", got, want)
	}
	if got, want := k.ProfitCents(), 18100-128000; got != want {
		t.Errorf("ProfitCents = %d, want %d", got, want)
	}
	if k.Profitable() {
		t.Error("亏钱的桶不该是 Profitable")
	}
	if got, want := k.AvgCupPrice(), "¥ 60.33"; got != want {
		t.Errorf("AvgCupPrice = %q, want %q", got, want)
	}
	if got, want := k.AvgMLPerCup(), 443; got != want {
		t.Errorf("AvgMLPerCup = %d, want %d", got, want)
	}
	if k.Active() || k.Status() != "停售" {
		t.Errorf("结桶后状态不对: %q", k.Status())
	}

	empty := Keg{CapacityML: 20000}
	if empty.AvgCupPrice() != "—" || empty.Status() != "待上架" || empty.SoldPct() != 0 {
		t.Errorf("空桶派生值不对: %q %q %d", empty.AvgCupPrice(), empty.Status(), empty.SoldPct())
	}
	over := Keg{CapacityML: 1000, SoldML: 1500}
	if over.RemainingML() != 0 || over.SoldPct() != 100 {
		t.Errorf("超卖时应收敛到 0ml / 100%% 走量: %d %d", over.RemainingML(), over.SoldPct())
	}

	tapped := time.Date(2026, 11, 24, 18, 0, 0, 0, time.Local)
	if (Keg{CapacityML: 20000, SoldML: 10000, TappedAt: &tapped}).IsSoldWarning() {
		t.Error("在售酒桶精确售出 50% 不应触发超 50% 告警")
	}
	if !(Keg{CapacityML: 20000, SoldML: 10200, TappedAt: &tapped}).IsSoldWarning() {
		t.Error("在售酒桶售出 51% 应触发超 50% 告警")
	}
	if (Keg{CapacityML: 20000, SoldML: 16000}).IsSoldWarning() {
		t.Error("未开桶即使有历史 SoldML 也不应触发在售酒桶告警")
	}
	if (Keg{CapacityML: 20000, SoldML: 16000, TappedAt: &tapped, FinishedAt: &finished}).IsSoldWarning() {
		t.Error("已结桶停售酒桶不应触发在售酒桶告警")
	}
}

func TestMoneyText(t *testing.T) {
	cases := []struct {
		cents int
		want  string
	}{
		{0, "¥ 0"}, {4500, "¥ 45"}, {4550, "¥ 45.50"}, {-109900, "-¥ 1099"}, {5, "¥ 0.05"},
	}
	for _, c := range cases {
		if got := platform.MoneyText(c.cents); got != c.want {
			t.Errorf("MoneyText(%d) = %q, want %q", c.cents, got, c.want)
		}
	}
}

func TestRetailMargin(t *testing.T) {
	r := RetailItem{PriceCents: 4200, CostCents: 2600, Stock: 12}
	if got, want := r.ProfitCents(), 1600; got != want {
		t.Errorf("ProfitCents = %d, want %d", got, want)
	}
	if got, want := r.Profit(), "¥ 16"; got != want {
		t.Errorf("Profit = %q, want %q", got, want)
	}
	if got, want := r.StockValue(), 31200; got != want {
		t.Errorf("StockValue = %d, want %d", got, want)
	}
}

func TestKegFormParsing(t *testing.T) {
	v := platform.ParseLocalTime("2026-11-16T10:00")
	if v == nil || v.Hour() != 10 {
		t.Errorf("ParseLocalTime datetime-local 失败: %v", v)
	}
	d := platform.ParseLocalTime("2026-11-16")
	if d == nil || d.Day() != 16 {
		t.Errorf("ParseLocalTime 日期失败: %v", d)
	}
	if platform.ParseLocalTime("") != nil {
		t.Error("空字符串应返回 nil")
	}
}
