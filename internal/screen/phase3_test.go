package screen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"pourops/internal/catalog"
	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/migrations"
)

type flushRecorder struct {
	*httptest.ResponseRecorder
}

func (f flushRecorder) Flush() {
	f.ResponseRecorder.Flush()
}

func TestScreenHubCloseUnblocksEvents(t *testing.T) {
	bus := event.NewEventBus()
	hub := NewHub(bus)
	svc := NewService(nil, hub, nil, nil, fstest.MapFS{})

	done := make(chan int, 1)
	go func() {
		rec := flushRecorder{httptest.NewRecorder()}
		req := httptest.NewRequest("GET", "/api/screen/events", nil)
		svc.Events(rec, req)
		done <- rec.Code
	}()

	time.Sleep(30 * time.Millisecond)
	hub.Close()

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("Events status = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Events handler did not exit promptly after hub.Close()")
	}
}

func TestScreenEventsMaxClientsCap(t *testing.T) {
	hub := NewHub(nil)
	defer hub.Close()

	chs := make([]chan struct{}, 0, maxScreenSSEClients)
	for range maxScreenSSEClients {
		ch, _, ok := hub.SubscribeChecked()
		if !ok {
			t.Fatalf("unexpected subscribe failure before reaching %d clients", maxScreenSSEClients)
		}
		chs = append(chs, ch)
	}
	defer func() {
		for _, ch := range chs {
			hub.Unsubscribe(ch)
		}
	}()

	svc := NewService(nil, hub, nil, nil, fstest.MapFS{})
	rec := flushRecorder{httptest.NewRecorder()}
	req := httptest.NewRequest("GET", "/api/screen/events", nil)
	svc.Events(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("65th SSE client status = %d, want 503", rec.Code)
	}
}

func TestScreenPlaylistZeroPriceAndSanitized500(t *testing.T) {
	pool := platform.NewTestPool(t, migrations.FS)
	ctx := context.Background()
	cfg := settings.NewSettingsStore(pool, "PourOps")
	if err := cfg.Refresh(ctx); err != nil {
		t.Fatalf("cfg.Refresh: %v", err)
	}
	svc := NewService(pool, nil, cfg, nil, fstest.MapFS{})

	// 写入一款挂屏在售但价格为 0 的酒，校验大屏返回 "" 而非 "¥0.00"
	if _, err := catalog.SaveBeer(ctx, pool, catalog.Beer{
		No: "01", Brewery: "测试厂", Name: "零元测试款", Style: "IPA", Category: "IPA",
		OnScreen: true, SortOrder: 1,
	}); err != nil {
		t.Fatalf("SaveBeer: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/screen/playlist", nil)
	svc.Playlist(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Playlist status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		Items []PlaylistItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal playlist: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("playlist items = %d, want 1", len(payload.Items))
	}
	if payload.Items[0].Price != "" || len(payload.Items[0].Cups) != 0 {
		t.Fatalf("expected empty price/cups for 0-cent beer, got price=%q cups=%+v", payload.Items[0].Price, payload.Items[0].Cups)
	}

	// 关闭连接池模拟数据库异常，校验 500 返回脱敏消息，不泄漏 pool/sql 错误
	pool.Close()
	recErr := httptest.NewRecorder()
	reqErr := httptest.NewRequest("GET", "/api/screen/playlist", nil)
	svc.Playlist(recErr, reqErr)
	if recErr.Code != http.StatusInternalServerError {
		t.Fatalf("Playlist closed pool status = %d, want 500", recErr.Code)
	}
	body := recErr.Body.String()
	if strings.Contains(strings.ToLower(body), "closed pool") || strings.Contains(body, "pgx") {
		t.Fatalf("Playlist 500 leaked internal DB error: %q", body)
	}
}
