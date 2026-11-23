package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/app"
	"pourops/internal/auth"
	"pourops/internal/event"
	"pourops/internal/lookup"
	"pourops/internal/platform"
	"pourops/internal/screen"
	"pourops/internal/settings"
	"pourops/migrations"
)

func main() {
	dsn := platform.Env("DATABASE_URL", "postgres://localhost:5432/bar?sslmode=disable")
	addr := platform.Env("ADDR", ":8080")
	brand := platform.Env("BRAND", "PourOps")
	secret := []byte(platform.Env("SESSION_SECRET", platform.RandomHex(32)))
	jiuheBaseURL := platform.Env("JIUHE_API_BASE", lookup.DefaultJiuheBaseURL)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}
	defer pool.Close()

	if err := platform.Migrate(ctx, pool, migrations.FS); err != nil {
		log.Fatalf("迁移失败: %v", err)
	}
	if err := auth.SeedBoss(ctx, pool); err != nil {
		log.Fatalf("初始化 BOSS 账号失败: %v", err)
	}

	settingsStore := settings.NewSettingsStore(pool, brand)
	if err := settingsStore.Refresh(ctx); err != nil {
		log.Fatalf("加载系统设置失败: %v", err)
	}

	bus := event.NewEventBus()
	webhooks := event.NewWebhookDispatcher(settingsStore, nil)
	webhooks.Attach(bus)
	screenHub := screen.NewHub(bus)

	assets, err := app.NewAssets()
	if err != nil {
		log.Fatalf("构建静态资源清单失败: %v", err)
	}
	renderer := app.NewRenderer(assets, settingsStore.Brand)

	handler := app.NewHandler(app.Deps{
		Pool:         pool,
		Settings:     settingsStore,
		Bus:          bus,
		Webhooks:     webhooks,
		ScreenHub:    screenHub,
		Assets:       assets,
		Renderer:     renderer,
		Secret:       secret,
		JiuheBaseURL: jiuheBaseURL,
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("监听 %s（后台 /admin，点餐 /pos，电视酒单 /screen/）", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP 服务关闭异常: %v", err)
	}
	if err := bus.Close(shutdownCtx); err != nil {
		log.Printf("事件总线队列排空异常: %v", err)
	}
}
