package settings_test

import (
	"context"
	"testing"

	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/migrations"
)

func TestSettingsStorePersistenceAndValidation(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()

	store := settings.NewSettingsStore(pool, "PourOps")
	if err := store.Refresh(ctx); err != nil {
		t.Fatalf("initial Refresh: %v", err)
	}
	if cfg := store.Pricing(); cfg.SellablePct != 80 || cfg.MarginPct != 50 {
		t.Fatalf("default pricing = %+v", cfg)
	}

	if _, err := store.Save(ctx, settings.StoreSettings{
		Brand:              "PourOps",
		DefaultSellablePct: 40,
		DefaultMarginPct:   50,
	}); err == nil {
		t.Fatal("should reject sellable < 50")
	}
	if _, err := store.Save(ctx, settings.StoreSettings{
		Brand:              "PourOps",
		DefaultSellablePct: 80,
		DefaultMarginPct:   50,
		WebhookURL:         "http://169.254.169.254/latest/meta-data",
	}); err == nil {
		t.Fatal("should block metadata SSRF URL")
	}

	saved, err := store.Save(ctx, settings.StoreSettings{
		Brand:              "北岸精酿",
		DefaultSellablePct: 85,
		DefaultMarginPct:   55,
		WebhookURL:         "https://example.com/hook",
		WebhookSecret:      "my-secret",
		WebhookEvents:      []string{string(event.EventOrderPaid), string(event.EventKegFinished)},
	})
	if err != nil {
		t.Fatalf("Save valid settings: %v", err)
	}
	if saved.Brand != "北岸精酿" || saved.DefaultSellablePct != 85 || saved.DefaultMarginPct != 55 {
		t.Fatalf("saved = %+v", saved)
	}

	// 另起实例从数据库加载，验证持久化生效
	reloaded := settings.NewSettingsStore(pool, "PourOps")
	if err := reloaded.Refresh(ctx); err != nil {
		t.Fatalf("reloaded Refresh: %v", err)
	}
	got := reloaded.Get()
	if got.Brand != "北岸精酿" || got.DefaultSellablePct != 85 || got.DefaultMarginPct != 55 ||
		got.WebhookURL != "https://example.com/hook" || len(got.WebhookEvents) != 2 {
		t.Fatalf("reloaded = %+v", got)
	}

	// 验证酒款分类增删持久化与重加载一致
	if _, err := reloaded.AddBeerCategory(ctx, "赛松"); err != nil {
		t.Fatalf("AddBeerCategory(赛松): %v", err)
	}
	if _, err := reloaded.DeleteBeerCategory(ctx, "皮尔森"); err != nil {
		t.Fatalf("DeleteBeerCategory(皮尔森): %v", err)
	}
	reloaded2 := settings.NewSettingsStore(pool, "PourOps")
	if err := reloaded2.Refresh(ctx); err != nil {
		t.Fatalf("reloaded2 Refresh: %v", err)
	}
	cats := reloaded2.BeerCategories()
	var hasSaison, hasPilsner bool
	for _, c := range cats {
		if c == "赛松" {
			hasSaison = true
		}
		if c == "皮尔森" {
			hasPilsner = true
		}
	}
	if !hasSaison || hasPilsner {
		t.Fatalf("persisted BeerCategories mismatch: %v", cats)
	}
	if _, err := reloaded2.ResetBeerCategories(ctx); err != nil {
		t.Fatalf("ResetBeerCategories: %v", err)
	}
	if len(reloaded2.BeerCategories()) != len(platform.DefaultBeerCategories) {
		t.Fatalf("ResetBeerCategories got %v", reloaded2.BeerCategories())
	}

	// 验证分模块保存（SaveGeneral / SaveWebhook）在数据库持久化层互不覆盖
	if _, err := reloaded2.SaveGeneral(ctx, "南岸酒馆", 88, 60); err != nil {
		t.Fatalf("SaveGeneral: %v", err)
	}
	if _, err := reloaded2.SaveWebhook(ctx, "https://example.com/v3", "sec-v3", false, []string{string(event.EventBeerTapped)}); err != nil {
		t.Fatalf("SaveWebhook: %v", err)
	}
	reloaded3 := settings.NewSettingsStore(pool, "PourOps")
	if err := reloaded3.Refresh(ctx); err != nil {
		t.Fatalf("reloaded3 Refresh: %v", err)
	}
	got3 := reloaded3.Get()
	if got3.Brand != "南岸酒馆" || got3.DefaultSellablePct != 88 || got3.DefaultMarginPct != 60 ||
		got3.WebhookURL != "https://example.com/v3" || got3.WebhookSecret != "sec-v3" ||
		len(got3.WebhookEvents) != 1 || got3.WebhookEvents[0] != string(event.EventBeerTapped) {
		t.Fatalf("isolated module persistence mismatch: %+v", got3)
	}
}
