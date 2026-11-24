package screen

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/catalog"
	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

type ScreenPage struct {
	ui.Layout
}

type PlaylistItem struct {
	No        string   `json:"no"`
	Brewery   string   `json:"brewery"`
	Name      string   `json:"name"`
	Style     string   `json:"style"`
	Category  string   `json:"category"`
	Tags      []string `json:"tags"`
	SalesTags []string `json:"sales_tags"`
	ABV       *float64 `json:"abv,omitempty"`
	IBU       *int     `json:"ibu,omitempty"`
	Size      string   `json:"size"`
	Price     string   `json:"price"`
	UT        *float64 `json:"ut_rating,omitempty"`
	UTCount   *int     `json:"ut_count,omitempty"`
	Poster    string   `json:"poster"`
	Sold      bool     `json:"sold"`
}

// Service 提供电视酒单静态屏与实时 SSE 接口。
type Service struct {
	pool     *pgxpool.Pool
	hub      *Hub
	settings *settings.SettingsStore
	renderer *ui.Renderer
	staticFS fs.FS
}

func NewService(pool *pgxpool.Pool, hub *Hub, cfg *settings.SettingsStore, renderer *ui.Renderer, staticFS fs.FS) *Service {
	return &Service{
		pool:     pool,
		hub:      hub,
		settings: cfg,
		renderer: renderer,
		staticFS: staticFS,
	}
}

func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/screen/playlist", s.Playlist)
	mux.HandleFunc("GET /api/screen/events", s.Events)
	mux.HandleFunc("GET /screen/", s.Page)
	mux.HandleFunc("GET /screen", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/screen/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /screen/playlist.json", s.PlaylistJSON)
}

func (s *Service) brand() string {
	if s.settings != nil {
		return s.settings.Brand()
	}
	return "PourOps"
}

func (s *Service) Page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/screen/" {
		http.NotFound(w, r)
		return
	}
	if s.renderer == nil {
		http.Error(w, "模板渲染器未初始化", http.StatusInternalServerError)
		return
	}
	s.renderer.RenderShell(w, r, "screen/screen.html", "screen", &ScreenPage{})
}

func (s *Service) PlaylistJSON(w http.ResponseWriter, r *http.Request) {
	if s.staticFS == nil {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(s.staticFS, "static/screen/playlist.json")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Service) Playlist(w http.ResponseWriter, r *http.Request) {
	beers, err := catalog.ListBeers(r.Context(), s.pool, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	items := make([]PlaylistItem, 0, len(beers))
	var cats []string
	if s.settings != nil {
		cats = s.settings.BeerCategories()
	}
	for _, b := range beers {
		if b.No == "" {
			continue
		}
		cat := platform.CategoryLabel(b.Category)
		if cat == "" {
			cat = platform.InferCategory(b.Style, cats)
		}
		items = append(items, PlaylistItem{
			No: b.No, Brewery: b.Brewery, Name: b.Name, Style: b.Style, Category: cat,
			Tags: b.Tags, SalesTags: b.SalesTags, ABV: b.ABV, IBU: b.IBU, Size: b.Size,
			Price: platform.PriceText(b.PriceCents), UT: b.UT, UTCount: b.UTCount, Poster: b.PosterURL,
			Sold: b.OnScreen,
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"brand":      s.brand(),
		"show_clock": true,
		"items":      items,
	})
}

func (s *Service) Events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := s.hub.Subscribe()
	defer s.hub.Unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			_, _ = w.Write([]byte("data: changed\n\n"))
			flusher.Flush()
		case <-ping.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}
