package catalog

import "testing"

func TestBeerFormFillsCupPrices(t *testing.T) {
	b := Beer{Price150: 2500, Price330: 4500}
	if got := b.Price150Input(); got != "25" {
		t.Errorf("150ml 回填 = %q", got)
	}
	if got := b.Price500Input(); got != "" {
		t.Errorf("没定价的 500ml 应该是空，得到 %q", got)
	}
}

func TestSyncPricePicks330OrCheapest(t *testing.T) {
	cases := []struct {
		name string
		b    Beer
		size string
		cent int
	}{
		{"有 330 用 330", Beer{Price150: 2500, Price330: 4500, Price500: 5000}, "150/330/500ml", 4500},
		{"150 比 500 贵时取 500", Beer{Price150: 5000, Price500: 2000}, "150/500ml", 2000},
		{"150 最便宜时取 150", Beer{Price150: 2000, Price500: 5000}, "150/500ml", 2000},
		{"只有 500 也能标价", Beer{Price500: 3000}, "500ml", 3000},
		{"没定价就清空", Beer{}, "", 0},
	}
	for _, c := range cases {
		c.b.SyncPrice()
		if c.b.Size != c.size || c.b.PriceCents != c.cent {
			t.Errorf("%s: size=%q price=%d, want %q/%d", c.name, c.b.Size, c.b.PriceCents, c.size, c.cent)
		}
	}
}

func TestCupPrices(t *testing.T) {
	b := Beer{Price150: 2500, Price330: 4500, Price500: 6500}
	cups := b.Cups()
	if len(cups) != 3 || cups[1].ML != 330 || cups[1].Cents != 4500 {
		t.Fatalf("三种杯型不对: %+v", cups)
	}
	b.SyncPrice()
	if b.Size != "150/330/500ml" || b.PriceCents != 4500 {
		t.Fatalf("折算后的酒单规格不对: %q %d", b.Size, b.PriceCents)
	}

	only := Beer{Price500: 4200}
	only.SyncPrice()
	if only.Size != "500ml" || only.PriceCents != 4200 {
		t.Fatalf("单一杯型折算不对: %q %d", only.Size, only.PriceCents)
	}
	if len(only.Cups()) != 1 {
		t.Fatalf("单一杯型应该只有 1 个: %+v", only.Cups())
	}

	none := Beer{Size: "330ml", PriceCents: 4500}
	none.SyncPrice()
	if none.Size != "" || none.PriceCents != 0 {
		t.Fatalf("没有定价时应该清空: %q %d", none.Size, none.PriceCents)
	}
}
