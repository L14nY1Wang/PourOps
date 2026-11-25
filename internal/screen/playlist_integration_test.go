package screen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"pourops/internal/catalog"
	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/migrations"
)

func TestPlaylistIncludesMultiCupPrices(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()

	abv68, abv52, abv42 := 6.8, 5.2, 4.2
	ibu72, ibu35 := 72, 35

	beers := []catalog.Beer{
		{
			No: "01", Brewery: "树屋", Name: "尤利乌斯", Style: "IPA - New England", Category: "IPA",
			ABV: &abv68, IBU: &ibu72, Price150: 2800, Price330: 5800, Price500: 7800,
			OnScreen: true, SortOrder: 1,
		},
		{
			No: "02", Brewery: "酿酒狗", Name: "朋克 IPA", Style: "IPA - American", Category: "IPA",
			ABV: &abv52, IBU: &ibu35, Price150: 2500, Price330: 4800,
			OnScreen: true, SortOrder: 2,
		},
		{
			No: "03", Brewery: "健力士", Name: "生啤", Style: "Stout - Irish Dry", Category: "世涛",
			ABV: &abv42, Price330: 4500,
			OnScreen: false, SortOrder: 3,
		},
	}
	for _, b := range beers {
		if _, err := catalog.SaveBeer(ctx, pool, b); err != nil {
			t.Fatalf("SaveBeer %s: %v", b.Name, err)
		}
	}

	bus := event.NewEventBus()
	defer bus.Close(ctx)
	svc := NewService(pool, NewHub(bus), settings.NewSettingsStore(nil, "北岸精酿"), nil, nil)

	rec := httptest.NewRecorder()
	svc.Playlist(rec, httptest.NewRequest(http.MethodGet, "/api/screen/playlist", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("Playlist status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Brand string         `json:"brand"`
		Items []PlaylistItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode playlist JSON: %v", err)
	}
	if got.Brand != "北岸精酿" || len(got.Items) != 3 {
		t.Fatalf("brand=%q items=%d, want 3", got.Brand, len(got.Items))
	}

	// 1. 三杯型
	it0 := got.Items[0]
	if it0.Size != "150/330/500ml" || it0.Price != "¥ 58" || !it0.Sold || len(it0.Cups) != 3 {
		t.Fatalf("item[0] = %+v", it0)
	}
	want0 := []PlaylistCup{
		{ML: 150, Size: "150ml", Price: "¥ 28"},
		{ML: 330, Size: "330ml", Price: "¥ 58"},
		{ML: 500, Size: "500ml", Price: "¥ 78"},
	}
	for i, w := range want0 {
		if it0.Cups[i] != w {
			t.Fatalf("item[0].Cups[%d] = %+v, want %+v", i, it0.Cups[i], w)
		}
	}

	// 2. 两杯型
	it1 := got.Items[1]
	if it1.Size != "150/330ml" || it1.Price != "¥ 48" || len(it1.Cups) != 2 {
		t.Fatalf("item[1] = %+v", it1)
	}
	if it1.Cups[0] != (PlaylistCup{ML: 150, Size: "150ml", Price: "¥ 25"}) ||
		it1.Cups[1] != (PlaylistCup{ML: 330, Size: "330ml", Price: "¥ 48"}) {
		t.Fatalf("item[1].Cups = %+v", it1.Cups)
	}

	// 3. 单杯型 + 售罄状态
	it2 := got.Items[2]
	if it2.Size != "330ml" || it2.Price != "¥ 45" || it2.Sold || len(it2.Cups) != 1 {
		t.Fatalf("item[2] = %+v", it2)
	}
	if it2.Cups[0] != (PlaylistCup{ML: 330, Size: "330ml", Price: "¥ 45"}) {
		t.Fatalf("item[2].Cups = %+v", it2.Cups)
	}
}
