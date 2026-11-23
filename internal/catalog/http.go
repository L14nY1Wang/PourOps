package catalog

import (
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

type CatalogListPage struct {
	ui.Layout
	Catalog      []CatalogBeer
	CatalogTotal int
	Q            string
	Cat          string
}

type CatalogFormPage struct {
	ui.Layout
	CatalogItem CatalogBeer
	CostInput   string
	Pricing     settings.PricingConfig
}

// Service 管理历史酒单列表、维护与导入导出 HTTP 接口。
type Service struct {
	pool     *pgxpool.Pool
	renderer *ui.Renderer
	settings *settings.SettingsStore
	codecs   *CodecRegistry
}

func NewService(pool *pgxpool.Pool, renderer *ui.Renderer, settings *settings.SettingsStore, codecs *CodecRegistry) *Service {
	if codecs == nil {
		codecs = NewDefaultCodecRegistry()
	}
	return &Service{
		pool:     pool,
		renderer: renderer,
		settings: settings,
		codecs:   codecs,
	}
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, guards ui.Guards) {
	mux.HandleFunc("GET /admin/beers/catalog", guards.Auth(s.CatalogList))
	mux.HandleFunc("GET /admin/beers/catalog/new", guards.Auth(s.CatalogNew))
	mux.HandleFunc("GET /admin/beers/catalog/{id}/edit", guards.Auth(s.CatalogEdit))
	mux.HandleFunc("POST /admin/beers/catalog", guards.Auth(s.CatalogSave))
	mux.HandleFunc("POST /admin/beers/catalog/{id}/delete", guards.Auth(s.CatalogDelete))
	mux.HandleFunc("GET /admin/beers/catalog/export", guards.Auth(s.CatalogExport))
	mux.HandleFunc("POST /admin/beers/catalog/import", guards.Auth(s.CatalogImport))
}

func (s *Service) currentPricing() settings.PricingConfig {
	if s.settings != nil {
		return s.settings.Pricing()
	}
	return settings.DefaultPricingConfig()
}

func (s *Service) currentBrand() string {
	if s.settings != nil {
		return s.settings.Brand()
	}
	return "PourOps"
}

func (s *Service) CatalogList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	cat, err := platform.ParseCategory(q.Get("cat"))
	if err != nil {
		cat = ""
	}
	ctx := r.Context()
	items, err := ListCatalogBeers(ctx, s.pool, query, cat)
	if err != nil {
		log.Printf("catalog list: %v", err)
		http.Error(w, "读取历史酒单失败", http.StatusInternalServerError)
		return
	}
	total, err := CountCatalogBeers(ctx, s.pool)
	if err != nil {
		log.Printf("catalog count: %v", err)
		http.Error(w, "统计历史酒单失败", http.StatusInternalServerError)
		return
	}
	s.renderer.Render(w, r, "admin/catalog.html", &CatalogListPage{
		Layout:       ui.Layout{Title: "历史酒单", Nav: "catalog", Msg: q.Get("msg"), Err: q.Get("err")},
		Catalog:      items,
		CatalogTotal: total,
		Q:            query,
		Cat:          cat,
	})
}

func (s *Service) CatalogNew(w http.ResponseWriter, r *http.Request) {
	s.renderer.Render(w, r, "admin/catalog_form.html", &CatalogFormPage{
		Layout:      ui.Layout{Title: "新增历史酒款", Nav: "catalog"},
		CatalogItem: CatalogBeer{CapacityML: DefaultCapacityML},
		Pricing:     s.currentPricing(),
	})
}

func (s *Service) CatalogEdit(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c, err := GetCatalogBeer(r.Context(), s.pool, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Redirect(w, r, "/admin/beers/catalog?err="+platform.URLMsg("找不到该历史酒款"), http.StatusSeeOther)
		return
	}
	if err != nil {
		log.Printf("catalog edit (%d): %v", id, err)
		http.Error(w, "读取历史酒款失败", http.StatusInternalServerError)
		return
	}
	s.renderer.Render(w, r, "admin/catalog_form.html", &CatalogFormPage{
		Layout:      ui.Layout{Title: "编辑历史酒款", Nav: "catalog"},
		CatalogItem: c,
		CostInput:   c.CostInput(),
		Pricing:     s.currentPricing(),
	})
}

func (s *Service) CatalogSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单格式不正确", http.StatusBadRequest)
		return
	}
	rawCost := strings.TrimSpace(r.FormValue("cost"))
	rawCap := strings.TrimSpace(r.FormValue("capacity_ml"))

	parsed, formErr := platform.ParseBeerForm(r)
	c := CatalogBeer{
		Brewery:   parsed.Brewery,
		Name:      parsed.Name,
		Style:     parsed.Style,
		Category:  parsed.Category,
		PosterURL: parsed.PosterURL,
		ABV:       parsed.ABV,
		IBU:       parsed.IBU,
		UT:        parsed.UT,
		UTCount:   parsed.UTCount,
		Price150:  parsed.Price150,
		Price330:  parsed.Price330,
		Price500:  parsed.Price500,
		Tags:      parsed.Tags,
	}
	if id := r.FormValue("id"); id != "" {
		c.ID, _ = strconv.ParseInt(id, 10, 64)
	}

	var capErr error
	if rawCap == "" {
		c.CapacityML = DefaultCapacityML
	} else {
		var capPtr *int
		capPtr, capErr = platform.ParseStrictInt(rawCap, "默认桶容量", 1, math.MaxInt32)
		if capErr == nil && capPtr != nil {
			c.CapacityML = *capPtr
		}
	}

	fail := func(status int, msg string) {
		title := "新增历史酒款"
		if c.ID > 0 {
			title = "编辑历史酒款"
		}
		w.WriteHeader(status)
		s.renderer.Render(w, r, "admin/catalog_form.html", &CatalogFormPage{
			Layout:      ui.Layout{Title: title, Nav: "catalog", Err: msg},
			CatalogItem: c,
			CostInput:   rawCost,
			Pricing:     s.currentPricing(),
		})
	}

	if formErr != nil {
		fail(http.StatusBadRequest, formErr.Error())
		return
	}
	if capErr != nil {
		fail(http.StatusBadRequest, capErr.Error())
		return
	}

	costCents, costErr := platform.MoneyCents(rawCost)
	if costErr != nil {
		fail(http.StatusBadRequest, "默认每桶进价："+costErr.Error())
		return
	}
	c.CostCents = costCents

	if _, err := SaveCatalogBeer(r.Context(), s.pool, c); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			fail(http.StatusBadRequest, msg)
			return
		}
		log.Printf("catalog save (%d): %v", c.ID, err)
		fail(http.StatusInternalServerError, "保存失败，请稍后重试")
		return
	}
	http.Redirect(w, r, "/admin/beers/catalog?msg="+platform.URLMsg("已保存到历史酒单"), http.StatusSeeOther)
}

func (s *Service) CatalogDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := DeleteCatalogBeer(r.Context(), s.pool, id); err != nil {
		log.Printf("catalog delete (%d): %v", id, err)
		http.Redirect(w, r, "/admin/beers/catalog?err="+platform.URLMsg("删除失败"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/beers/catalog?msg="+platform.URLMsg("已从历史酒单移除"), http.StatusSeeOther)
}

func (s *Service) CatalogExport(w http.ResponseWriter, r *http.Request) {
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "csv"
	}
	codec, ok := s.codecs.Find(format)
	if !ok {
		http.Error(w, "不支持的导出格式（仅支持 csv 或 json）", http.StatusBadRequest)
		return
	}

	items, err := ListCatalogBeers(r.Context(), s.pool, "", "")
	if err != nil {
		log.Printf("catalog export: %v", err)
		http.Error(w, "导出历史酒单失败", http.StatusInternalServerError)
		return
	}

	data, err := codec.Encode(items, s.currentBrand(), time.Now())
	if err != nil {
		log.Printf("catalog export %s: %v", format, err)
		http.Error(w, "生成导出文件失败", http.StatusInternalServerError)
		return
	}

	stamp := time.Now().In(platform.ReportZone).Format("20060102")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", codec.ContentType())
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="beer-catalog-%s%s"`, stamp, codec.FileExt()))
	_, _ = w.Write(data)
}

func (s *Service) CatalogImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, ImportMaxBytes+64*1024)
	if err := r.ParseMultipartForm(ImportMaxBytes); err != nil {
		http.Redirect(w, r, "/admin/beers/catalog?err="+platform.URLMsg("上传文件过大或格式错误（上限 5MB）"), http.StatusSeeOther)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Redirect(w, r, "/admin/beers/catalog?err="+platform.URLMsg("请先选择要导入的 CSV 或 JSON 文件"), http.StatusSeeOther)
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, ImportMaxBytes+1))
	if err != nil {
		http.Redirect(w, r, "/admin/beers/catalog?err="+platform.URLMsg("读取上传文件失败"), http.StatusSeeOther)
		return
	}
	if len(raw) > ImportMaxBytes {
		http.Redirect(w, r, "/admin/beers/catalog?err="+platform.URLMsg("导入文件不能超过 5MB"), http.StatusSeeOther)
		return
	}

	items, err := s.codecs.Decode(hdr.Filename, raw)
	if err != nil {
		http.Redirect(w, r, "/admin/beers/catalog?err="+platform.URLMsg(err.Error()), http.StatusSeeOther)
		return
	}
	mode := strings.TrimSpace(r.FormValue("mode"))
	res, err := ImportCatalogBeers(r.Context(), s.pool, items, mode)
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			http.Redirect(w, r, "/admin/beers/catalog?err="+platform.URLMsg("导入失败："+msg), http.StatusSeeOther)
			return
		}
		log.Printf("catalog import: %v", err)
		http.Redirect(w, r, "/admin/beers/catalog?err="+platform.URLMsg("导入失败，请稍后重试"), http.StatusSeeOther)
		return
	}
	msg := fmt.Sprintf("导入完成：新增 %d 款，更新 %d 款，跳过 %d 款", res.Inserted, res.Updated, res.Skipped)
	http.Redirect(w, r, "/admin/beers/catalog?msg="+platform.URLMsg(msg), http.StatusSeeOther)
}
