package inventory

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/event"
	"pourops/internal/settings"
	"pourops/internal/ui"
	"pourops/web"
)

func testRenderer(t *testing.T) *ui.Renderer {
	t.Helper()
	return ui.MustRenderer(web.TemplatesFS, nil, func() string { return "PourOps" })
}

func newTestService(t *testing.T, pool *pgxpool.Pool) (*Service, *settings.SettingsStore, *event.EventBus) {
	t.Helper()
	cfg := settings.NewSettingsStore(pool, "PourOps")
	bus := event.NewEventBus()
	return NewService(pool, testRenderer(t), cfg, bus), cfg, bus
}
