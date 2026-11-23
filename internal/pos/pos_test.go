package pos

import "testing"

func TestSpotAndBillMath(t *testing.T) {
	if got := SpotLabel("bar-12"); got != "吧台 12" {
		t.Errorf("SpotLabel(bar-12) = %q", got)
	}
	if got := SpotLabel("table-3"); got != "小桌 3" {
		t.Errorf("SpotLabel(table-3) = %q", got)
	}
	it := OrderItem{Kind: "beer", UnitCents: 4500, Qty: 2, ML: 330}
	if it.Amount() != 9000 || it.AmountText() != "¥ 90" || it.QtyText() != "×2" {
		t.Errorf("账单行 = %d / %q / %q", it.Amount(), it.AmountText(), it.QtyText())
	}
	if got := (OrderItem{Kind: "retail", UnitCents: 4200, Qty: 1}).Detail(); got != "¥ 42" {
		t.Errorf("零售详情 = %q", got)
	}
	o := OrderView{Unpaid: []OrderItem{it, {Kind: "retail", UnitCents: 4200, Qty: 3}}, Total: 9000 + 12600}
	if o.TotalText() != "¥ 216" {
		t.Errorf("合计 = %q", o.TotalText())
	}
}

func TestUnpaidGroupsMergeCups(t *testing.T) {
	o := OrderView{Unpaid: []OrderItem{
		{Name: "酿酒狗 · 朋克 IPA", Qty: 2, UnitCents: 2500},
		{Name: "内华达 · 淡色艾尔", Qty: 1, UnitCents: 4200},
		{Name: "酿酒狗 · 朋克 IPA", Qty: 1, UnitCents: 6500},
	}}
	groups := o.UnpaidGroups()
	if len(groups) != 2 {
		t.Fatalf("同款酒应该并成一条，得到 %d 条", len(groups))
	}
	if groups[0].Name != "酿酒狗 · 朋克 IPA" || len(groups[0].Lines) != 2 {
		t.Fatalf("第一条应该是朋克 IPA 的两个杯型: %+v", groups[0])
	}
	if got := groups[0].TotalText(); got != "¥ 115" {
		t.Fatalf("合并后合计 = %q", got)
	}
	if got := groups[1].TotalText(); got != "¥ 42" {
		t.Fatalf("第二条合计 = %q", got)
	}
}
