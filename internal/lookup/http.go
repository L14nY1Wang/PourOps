package lookup

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"pourops/internal/platform"
	"pourops/internal/ui"
)

// Service 聚合多个 Provider，按注册顺序返回搜索结果并支持故障降级。
type Service struct {
	providers []Provider
}

func NewService(providers ...Provider) *Service {
	var clean []Provider
	for _, p := range providers {
		if p != nil {
			clean = append(clean, p)
		}
	}
	return &Service{providers: clean}
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, guards ui.Guards) {
	mux.HandleFunc("GET /admin/beers/lookup", guards.Auth(s.LookupBeers))
	mux.HandleFunc("GET /admin/beers/lookup/detail", guards.Auth(s.LookupBeerDetail))
}

// remoteLookupTimeout 是远程源（酒核等）的单次检索上限；本地 catalog 不受此限制，
// 保证慢/挂的上游最多拖慢响应 2.5s，而不是等满客户端超时。
const remoteLookupTimeout = 2500 * time.Millisecond

type providerResult struct {
	provider Provider
	items    []Candidate
	err      error
}

// searchConcurrent 并发调用所有已启用 provider，结果按注册顺序回填以保持合并顺序稳定。
// 非 catalog 远程源使用独立的超时 context；provider 需自行响应 ctx 取消。
func (s *Service) searchConcurrent(ctx context.Context, q string, catalogLimit, remoteLimit int) []providerResult {
	targets := make([]Provider, 0, len(s.providers))
	for _, p := range s.providers {
		if !p.Enabled() || (q == "" && !p.SupportsEmptyQuery()) {
			continue
		}
		targets = append(targets, p)
	}

	results := make([]providerResult, len(targets))
	var wg sync.WaitGroup
	for i, p := range targets {
		results[i].provider = p
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.Name() == "catalog" {
				results[i].items, results[i].err = p.Search(ctx, q, catalogLimit)
				return
			}
			pctx, cancel := context.WithTimeout(ctx, remoteLookupTimeout)
			defer cancel()
			results[i].items, results[i].err = p.Search(pctx, q, remoteLimit)
		}()
	}
	wg.Wait()
	return results
}

func (s *Service) LookupBeers(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	ctx := r.Context()

	if q == "" {
		items := make([]Candidate, 0, 16)
		for _, res := range s.searchConcurrent(ctx, "", 12, 12) {
			if res.err != nil {
				log.Printf("beer lookup empty (%s): %v", res.provider.Name(), res.err)
				http.Error(w, "查询历史酒单失败", http.StatusInternalServerError)
				return
			}
			items = append(items, res.items...)
		}
		writeJSON(w, http.StatusOK, Response{Items: items})
		return
	}

	items := make([]Candidate, 0, 16)
	var warnings []string
	for _, res := range s.searchConcurrent(ctx, q, 6, 10) {
		if res.err != nil {
			log.Printf("beer lookup %s (%q): %v", res.provider.Name(), q, res.err)
			if res.provider.Name() != "catalog" {
				warnings = append(warnings, res.provider.Name())
			}
			continue
		}
		items = append(items, res.items...)
	}

	var warning string
	if len(warnings) > 0 {
		if len(items) > 0 {
			warning = "在线 UT 库暂时不可用，已展示本店历史酒单结果"
		} else {
			warning = "在线 UT 库暂时不可用，可先手动填写酒款信息"
		}
	}
	writeJSON(w, http.StatusOK, Response{
		Items:   items,
		Warning: warning,
	})
}

func (s *Service) LookupBeerDetail(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source == "" || source == "db" {
		source = "catalog"
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))

	var target Provider
	for _, p := range s.providers {
		if p.Name() == source {
			target = p
			break
		}
	}
	if target == nil {
		http.Error(w, "不支持的数据源", http.StatusBadRequest)
		return
	}
	if !target.Enabled() {
		http.Error(w, target.Label()+"未启用", http.StatusServiceUnavailable)
		return
	}

	detail, err := target.Detail(r.Context(), id)
	if errors.Is(err, ErrInvalidID) {
		http.Error(w, "无效的酒款 ID", http.StatusBadRequest)
		return
	}
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "未找到该酒款详情", http.StatusNotFound)
		return
	}
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			http.Error(w, msg, http.StatusServiceUnavailable)
			return
		}
		log.Printf("beer lookup %s detail (%s): %v", source, id, err)
		if source == "catalog" {
			http.Error(w, "读取酒款详情失败", http.StatusInternalServerError)
		} else {
			http.Error(w, "获取"+target.Label()+"详情失败", http.StatusBadGateway)
		}
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
