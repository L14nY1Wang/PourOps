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

	now := time.Now()
	stamp := now.In(platform.ReportZone).Format("20060102")
	if zc, isZIP := codec.(ZIPCodec); isZIP {
		prepared, err := zc.PrepareExport(items, s.currentBrand(), now)
		if err != nil {
			if msg, ok := platform.UserErrorMessage(err); ok {
				http.Error(w, msg, http.StatusBadRequest)
				return
			}
			log.Printf("catalog export zip prepare: %v", err)
			http.Error(w, "生成历史酒单压缩包失败", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", codec.ContentType())
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="beer-catalog-%s%s"`, stamp, codec.FileExt()))
		if err := prepared.WriteTo(w); err != nil {
			log.Printf("catalog export zip stream: %v", err)
		}
		return
	}

	data, err := codec.Encode(items, s.currentBrand(), now)
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		log.Printf("catalog export zip: %v", err)
		http.Error(w, "生成历史酒单压缩包失败", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", codec.ContentType())
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="beer-catalog-%s%s"`, stamp, codec.FileExt()))
	_, _ = w.Write(data)
}

func (s *Service) CatalogImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, ImportArchiveMaxBytes+64*1024)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		platform.RedirectErr(w, r, "/admin/settings", "上传压缩包过大或格式错误（上限 50MB）")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		platform.RedirectErr(w, r, "/admin/settings", "请先选择要导入的 .zip 历史酒单压缩包")
		return
	}
	defer file.Close()

	zipCodec, ok := s.codecs.Find("zip")
	if !ok {
		platform.RedirectErr(w, r, "/admin/settings", "压缩包解码器未启用")
		return
	}

	lowerName := strings.ToLower(strings.TrimSpace(hdr.Filename))
	var items []ImportItem
	decoded := false
	if ra, hasRA := file.(io.ReaderAt); hasRA {
		if dec, hasDecRA := zipCodec.(interface {
			DecodeReaderAt(io.ReaderAt, int64) ([]ImportItem, error)
		}); hasDecRA {
			size := hdr.Size
			if rs, ok := file.(io.Seeker); ok {
				if end, serr := rs.Seek(0, io.SeekEnd); serr == nil {
					size = end
					_, _ = rs.Seek(0, io.SeekStart)
				}
			}
			if size > ImportArchiveMaxBytes {
				platform.RedirectErr(w, r, "/admin/settings", "导入压缩包不能超过 50MB")
				return
			}
			var head [4]byte
			n, _ := ra.ReadAt(head[:], 0)
			if !strings.HasSuffix(lowerName, ".zip") && !hasZIPMagic(head[:n]) {
				platform.RedirectErr(w, r, "/admin/settings", "请上传本系统导出的 .zip 历史酒单压缩包")
				return
			}
			items, err = dec.DecodeReaderAt(ra, size)
			decoded = true
		}
	}
	if !decoded {
		raw, rerr := io.ReadAll(io.LimitReader(file, ImportArchiveMaxBytes+1))
		if rerr != nil {
			platform.RedirectErr(w, r, "/admin/settings", "读取上传压缩包失败")
			return
		}
		if len(raw) > ImportArchiveMaxBytes {
			platform.RedirectErr(w, r, "/admin/settings", "导入压缩包不能超过 50MB")
			return
		}
		if !strings.HasSuffix(lowerName, ".zip") && !hasZIPMagic(raw) {
			platform.RedirectErr(w, r, "/admin/settings", "请上传本系统导出的 .zip 历史酒单压缩包")
			return
		}
		items, err = zipCodec.Decode(raw)
	}
	if err != nil {
		platform.RedirectErr(w, r, "/admin/settings", err.Error())
		return
	}

	mode := strings.TrimSpace(r.FormValue("mode"))
	res, err := ImportCatalogBeersWithPosters(r.Context(), s.pool, s.posters, items, mode, s.currentCategories())
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, "/admin/settings", "导入失败："+msg)
			return
		}
		log.Printf("catalog import: %v", err)
		platform.RedirectErr(w, r, "/admin/settings", "导入失败，请稍后重试")
		return
	}
	msg := fmt.Sprintf("导入完成：新增 %d 款，更新 %d 款，跳过 %d 款", res.Inserted, res.Updated, res.Skipped)
	platform.RedirectMsg(w, r, "/admin/settings", msg)
}
