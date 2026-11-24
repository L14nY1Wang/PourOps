package lookup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"pourops/internal/catalog"
	"pourops/internal/platform"
	"pourops/migrations"
)

func ptrF(v float64) *float64 { return &v }

func TestLookupCatalogRankingAndDetail(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	svc := NewService(NewLocalCatalogProvider(pool))
	ctx := context.Background()

	seed := []catalog.CatalogBeer{
		{Brewery: "Test", Name: "我的100%测试", Style: "Lager", Tags: []string{"麦香"}, CapacityML: 20000},
		{Brewery: "BrewDog", Name: "Double Punk IPA", Style: "IPA", Tags: []string{"苦爽"}, CapacityML: 20000},
		{Brewery: "BrewDog", Name: "Punk IPA Post Modern", Style: "IPA", Tags: []string{"西柚"}, CapacityML: 20000},
		{Brewery: "BrewDog", Name: "Punk", Style: "IPA - American", Category: "IPA", ABV: ptrF(5.4), Price330: 4500, CapacityML: 30000, CostCents: 120000, PosterURL: "https://assets.untappd.com/punk.jpg"},
	}
	var punkID int64
	for _, c := range seed {
		id, err := catalog.SaveCatalogBeer(ctx, pool, c)
		if err != nil {
			t.Fatal(err)
		}
		if c.Name == "Punk" {
			punkID = id
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/beers/lookup?q=punk", nil)
	rec := httptest.NewRecorder()
	svc.LookupBeers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup status = %d", rec.Code)
	}
	var res Response
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 3 {
		t.Fatalf("期望 3 条匹配，实际 %d: %+v", len(res.Items), res.Items)
	}
	if res.Items[0].Name != "Punk" || res.Items[1].Name != "Punk IPA Post Modern" || res.Items[2].Name != "Double Punk IPA" {
		t.Fatalf("搜索排序不对: %+v", res.Items)
	}
	if res.Items[0].Source != "catalog" || !res.Items[0].HasPricing {
		t.Fatalf("候选项字段不对: %+v", res.Items[0])
	}

	reqPct := httptest.NewRequest(http.MethodGet, "/admin/beers/lookup?q=%25", nil)
	recPct := httptest.NewRecorder()
	svc.LookupBeers(recPct, reqPct)
	var resPct Response
	_ = json.Unmarshal(recPct.Body.Bytes(), &resPct)
	if len(resPct.Items) != 1 || resPct.Items[0].Name != "我的100%测试" {
		t.Fatalf("%% 搜索应字面量匹配，实际 %+v", resPct.Items)
	}

	reqEmpty := httptest.NewRequest(http.MethodGet, "/admin/beers/lookup?q=", nil)
	recEmpty := httptest.NewRecorder()
	svc.LookupBeers(recEmpty, reqEmpty)
	var resEmpty Response
	_ = json.Unmarshal(recEmpty.Body.Bytes(), &resEmpty)
	if len(resEmpty.Items) != 4 {
		t.Fatalf("空查询应返回全部 4 条，实际 %d", len(resEmpty.Items))
	}

	reqDet := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/beers/lookup/detail?source=catalog&id=%d", punkID), nil)
	recDet := httptest.NewRecorder()
	svc.LookupBeerDetail(recDet, reqDet)
	if recDet.Code != http.StatusOK {
		t.Fatalf("detail status = %d", recDet.Code)
	}
	var det Detail
	_ = json.Unmarshal(recDet.Body.Bytes(), &det)
	if det.SourceLabel != "本店历史酒单" || det.Category != "IPA" || det.CapacityML != "30000" || det.Cost != "1200" || det.Price330 != "45" {
		t.Fatalf("detail 不对: %+v", det)
	}
}

func TestLookupLocalCatalogWithCustomCategoriesFn(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	id, err := catalog.SaveCatalogBeerWithCategories(ctx, pool, CatalogBeerWithSaison(), []string{"IPA", "赛松"})
	if err != nil {
		t.Fatal(err)
	}
	lp := NewLocalCatalogProvider(pool)
	lp.SetCategoriesFn(func() []string { return []string{"IPA", "赛松"} })
	svc := NewService(lp)
	rec := httptest.NewRecorder()
	svc.LookupBeerDetail(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/beers/lookup/detail?source=catalog&id=%d", id), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var det Detail
	if err := json.Unmarshal(rec.Body.Bytes(), &det); err != nil {
		t.Fatal(err)
	}
	if det.Category != "赛松" {
		t.Fatalf("自定义分类 detail.Category = %q, want 赛松", det.Category)
	}
}

func CatalogBeerWithSaison() catalog.CatalogBeer {
	return catalog.CatalogBeer{Brewery: "农舍厂", Name: "比利时赛松", Style: "比利时赛松 / Saison", CapacityML: 20000}
}
