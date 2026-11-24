package catalog

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

// Service 提供本地酒标静态访问与系统设置内的历史酒单压缩包导入导出接口。
type Service struct {
	pool     *pgxpool.Pool
	settings *settings.SettingsStore
	codecs   *CodecRegistry
	posters  *PosterStore
}

func NewService(pool *pgxpool.Pool, settings *settings.SettingsStore, codecs *CodecRegistry, posters *PosterStore) *Service {
	if codecs == nil {
		var catsFn func() []string
		if settings != nil {
			catsFn = settings.BeerCategories
		}
		codecs = NewDefaultCodecRegistryWithOptions(catsFn, posters)
	}
	return &Service{
		pool:     pool,
		settings: settings,
		codecs:   codecs,
		posters:  posters,
	}
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, guards ui.Guards) {
	mux.HandleFunc("GET /posters/{name}", s.ServePoster)
	mux.HandleFunc("GET /admin/settings/catalog/export", guards.Boss(s.CatalogExport))
	mux.HandleFunc("POST /admin/settings/catalog/import", guards.Boss(s.CatalogImport))
}

func (s *Service) ServePoster(w http.ResponseWriter, r *http.Request) {
	if s.posters == nil {
		http.NotFound(w, r)
		return
	}
	s.posters.ServePoster(w, r)
}

func (s *Service) currentBrand() string {
	if s.settings != nil {
		return s.settings.Brand()
	}
	return "PourOps"
}

func (s *Service) currentCategories() []string {
	if s.settings != nil {
		return s.settings.BeerCategories()
	}
	return platform.DefaultBeerCategories
}

func (s *Service) CatalogExport(w http.ResponseWriter, r *http.Request) {
	codec, ok := s.codecs.Find("zip")
	if !ok {
		http.Error(w, "历史酒单压缩包编码器未启用", http.StatusInternalServerError)
		return
	}

	items, err := ListCatalogBeers(r.Context(), s.pool, "", "")
	if err != nil {
		log.Printf("catalog export: %v", err)
		http.Error(w, "导出历史酒单压缩包失败", http.StatusInternalServerError)
		return
	}

	// 导出前尽力把仍保留远程链接的酒标拉取到本地，确保打包后的压缩包完全自包含。
	LocalizeCatalogPosters(r.Context(), s.pool, s.posters, items)

	data, err := codec.Encode(items, s.currentBrand(), time.Now())
	if err != nil {
		log.Printf("catalog export zip: %v", err)
		http.Error(w, "生成历史酒单压缩包失败", http.StatusInternalServerError)
		return
	}

	stamp := time.Now().In(platform.ReportZone).Format("20060102")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", codec.ContentType())
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="beer-catalog-%s%s"`, stamp, codec.FileExt()))
	_, _ = w.Write(data)
}

func (s *Service) CatalogImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, ImportArchiveMaxBytes+64*1024)
	if err := r.ParseMultipartForm(ImportArchiveMaxBytes); err != nil {
		http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg("上传压缩包过大或格式错误（上限 50MB）"), http.StatusSeeOther)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg("请先选择要导入的 .zip 历史酒单压缩包"), http.StatusSeeOther)
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, ImportArchiveMaxBytes+1))
	if err != nil {
		http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg("读取上传压缩包失败"), http.StatusSeeOther)
		return
	}
	if len(raw) > ImportArchiveMaxBytes {
		http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg("导入压缩包不能超过 50MB"), http.StatusSeeOther)
		return
	}

	lowerName := strings.ToLower(strings.TrimSpace(hdr.Filename))
	if !strings.HasSuffix(lowerName, ".zip") && !hasZIPMagic(raw) {
		http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg("请上传本系统导出的 .zip 历史酒单压缩包"), http.StatusSeeOther)
		return
	}
	zipCodec, ok := s.codecs.Find("zip")
	if !ok {
		http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg("压缩包解码器未启用"), http.StatusSeeOther)
		return
	}
	items, err := zipCodec.Decode(raw)
	if err != nil {
		http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg(err.Error()), http.StatusSeeOther)
		return
	}

	mode := strings.TrimSpace(r.FormValue("mode"))
	res, err := ImportCatalogBeersWithPosters(r.Context(), s.pool, s.posters, items, mode, s.currentCategories())
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg("导入失败："+msg), http.StatusSeeOther)
			return
		}
		log.Printf("catalog import: %v", err)
		http.Redirect(w, r, "/admin/settings?err="+platform.URLMsg("导入失败，请稍后重试"), http.StatusSeeOther)
		return
	}
	msg := fmt.Sprintf("导入完成：新增 %d 款，更新 %d 款，跳过 %d 款", res.Inserted, res.Updated, res.Skipped)
	http.Redirect(w, r, "/admin/settings?msg="+platform.URLMsg(msg), http.StatusSeeOther)
}
