package settings

import (
	"context"
	"strings"
	"sync"
	"testing"

	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/migrations"
)

func TestSaveCombinedPreservesCategoriesAndWebhookFields(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	store := NewSettingsStore(pool, "PourOps")
	if err := store.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	cats, err := store.AddBeerCategory(ctx, "海盐古斯")
	if err != nil {
		t.Fatalf("AddBeerCategory: %v", err)
	}
	wantCatCount := len(cats)

	saved, err := store.SaveCombined(
		ctx,
		"NewBrand",
		85,
		60,
		"https://example.com/hook",
		"sec-init",
		false,
		[]string{string(event.EventOrderPaid)},
		true,
	)
	if err != nil {
		t.Fatalf("SaveCombined initial: %v", err)
	}
	if saved.Brand != "NewBrand" || saved.DefaultSellablePct != 85 || saved.DefaultMarginPct != 60 {
		t.Fatalf("unexpected general settings: %+v", saved)
	}
	if saved.WebhookSecret != "sec-init" || len(saved.WebhookEvents) != 1 || saved.WebhookEvents[0] != string(event.EventOrderPaid) {
		t.Fatalf("unexpected webhook settings: %+v", saved)
	}
	if got := len(store.BeerCategories()); got != wantCatCount {
		t.Fatalf("categories overwritten by SaveCombined: got %d, want %d", got, wantCatCount)
	}

	// 空 secret 且 hasEventsField=false 时保留现有密钥与订阅事件
	updated, err := store.SaveCombined(ctx, "NewBrand2", 80, 55, "https://example.com/hook", "", false, nil, false)
	if err != nil {
		t.Fatalf("SaveCombined preserve: %v", err)
	}
	if updated.WebhookSecret != "sec-init" || len(updated.WebhookEvents) != 1 {
		t.Fatalf("expected secret and events preserved, got secret=%q events=%v", updated.WebhookSecret, updated.WebhookEvents)
	}

	// clearSecret=true 且 hasEventsField=true 传空切片时清空密钥与事件
	cleared, err := store.SaveCombined(ctx, "NewBrand2", 80, 55, "https://example.com/hook", "", true, nil, true)
	if err != nil {
		t.Fatalf("SaveCombined clear: %v", err)
	}
	if cleared.WebhookSecret != "" || len(cleared.WebhookEvents) != 0 {
		t.Fatalf("expected secret and events cleared, got secret=%q events=%v", cleared.WebhookSecret, cleared.WebhookEvents)
	}

	// 非法 URL 拒绝且不破坏现有品牌
	if _, err := store.SaveCombined(ctx, "BadBrand", 80, 55, "http://127.0.0.1:9999/hook", "", false, nil, false); err == nil {
		t.Fatalf("expected loopback webhook URL rejected")
	}
	if got := store.Brand(); got != "NewBrand2" {
		t.Fatalf("brand mutated on failed SaveCombined: got %q", got)
	}
}

func TestSaveCombinedConcurrentWithCategories(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	store := NewSettingsStore(pool, "PourOps")
	if err := store.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	errCh := make(chan error, 12)
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			_, err := store.SaveCombined(ctx, "PourOps", 80, 50+n, "", "", false, nil, false)
			errCh <- err
		}(i)
		go func(n int) {
			defer wg.Done()
			name := "并发分类" + string(rune('A'+n))
			_, err := store.AddBeerCategory(ctx, name)
			if err != nil && !strings.Contains(err.Error(), "已存在") {
				errCh <- err
				return
			}
			errCh <- nil
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent update error: %v", err)
		}
	}

	fresh := NewSettingsStore(pool, "PourOps")
	if err := fresh.Refresh(ctx); err != nil {
		t.Fatalf("fresh.Refresh: %v", err)
	}
	cats := fresh.BeerCategories()
	for i := range 6 {
		want := "并发分类" + string(rune('A'+i))
		found := false
		for _, c := range cats {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("lost category %q after concurrent SaveCombined; cats=%v", want, cats)
		}
	}
}
