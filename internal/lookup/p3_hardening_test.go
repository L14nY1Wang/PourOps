package lookup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pourops/internal/platform"
)

// 慢上游（远超 2.5s 超时）不应拖住本地 catalog 结果。
func TestLookupBeersCatalogFastWithSlowUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(10 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	svc := NewService(
		stubProvider{
			name: "catalog", label: "本店", enabled: true, empty: true,
			items: []Candidate{{Source: "catalog", ID: "1", Name: "本地酒"}},
		},
		NewJiuheProvider(srv.URL+"/api", srv.Client()),
	)

	start := time.Now()
	rec := httptest.NewRecorder()
	svc.LookupBeers(rec, httptest.NewRequest(http.MethodGet, "/admin/beers/lookup?q=IPA", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var res Response
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Name != "本地酒" {
		t.Fatalf("慢上游不应吞掉本地结果: %+v", res.Items)
	}
	if res.Warning == "" {
		t.Fatal("上游超时应返回降级告警")
	}
	if elapsed > 4*time.Second {
		t.Fatalf("响应被慢上游拖住: %v，应被 2.5s 超时截断", elapsed)
	}
}

// 缓存整份结果并按 limit 切片，不同 limit 只请求一次上游。
func TestJiuheSearchCacheSlicesByLimit(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"items":[
			  {"product":{"id":1,"name":"甲 IPA"},"wine":{"id":11,"nameZh":"甲 IPA","brand":{"nameZh":"厂A"}}},
			  {"product":{"id":2,"name":"乙 IPA"},"wine":{"id":12,"nameZh":"乙 IPA","brand":{"nameZh":"厂B"}}},
			  {"product":{"id":3,"name":"丙 IPA"},"wine":{"id":13,"nameZh":"丙 IPA","brand":{"nameZh":"厂C"}}}
			]}`)),
		}, nil
	})}
	p := NewJiuheProvider("https://mock.jiuhe.test/api", client)
	ctx := context.Background()

	for _, tc := range []struct {
		q     string
		limit int
		want  int
	}{
		{"ipa", 10, 3},
		{" IPA ", 2, 2},
		{"ipa", 0, 3},
	} {
		got, err := p.Search(ctx, tc.q, tc.limit)
		if err != nil || len(got) != tc.want {
			t.Fatalf("q=%q limit=%d 应返回 %d 条: err=%v got=%d", tc.q, tc.limit, tc.want, err, len(got))
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("不同 limit 应复用同一次上游请求，实际 %d 次", n)
	}

	// 调用方改返回值不能污染缓存。
	first, err := p.Search(ctx, "ipa", 0)
	if err != nil || len(first) != 3 {
		t.Fatalf("二次查询应命中缓存: err=%v len=%d", err, len(first))
	}
	first[0].Name = "被改了"
	if got, _ := p.Search(ctx, "ipa", 0); got[0].Name == "被改了" {
		t.Fatal("返回值与缓存共用底层数组，调用方修改污染了缓存")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("命中缓存后不应再请求上游，实际 %d", n)
	}
}

// 连续 5xx 后 30s 内直接失败，不再发起请求；成功或 404 不应误判为上游宕机。
func TestJiuheFailureCooldown(t *testing.T) {
	var calls atomic.Int32
	var down atomic.Bool
	down.Store(true)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		if down.Load() {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"down"}`)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"items":[]}`)),
		}, nil
	})}

	p := NewJiuheProvider("https://mock.jiuhe.test/api", client)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	ctx := context.Background()

	for _, q := range []string{"a", "b"} {
		if _, err := p.Search(ctx, q, 10); err == nil {
			t.Fatalf("%q: 上游宕机应返回错误", q)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("前两次失败应各请求一次，实际 %d", n)
	}

	if _, err := p.Search(ctx, "c", 10); !errors.Is(err, errJiuheCoolingDown) {
		t.Fatalf("冷却期内应快速失败，实际 err=%v", err)
	}
	now = now.Add(29 * time.Second)
	if _, err := p.Search(ctx, "d", 10); !errors.Is(err, errJiuheCoolingDown) {
		t.Fatalf("29s 内应仍在冷却，实际 err=%v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("冷却期内不应发起请求，实际 %d", n)
	}

	now = now.Add(2 * time.Second)
	down.Store(false)
	if _, err := p.Search(ctx, "e", 10); err != nil {
		t.Fatalf("冷却结束后应恢复请求: %v", err)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("冷却结束后应重新请求上游，实际 %d", n)
	}
	if _, err := p.Search(ctx, "f", 10); err != nil || calls.Load() != 4 {
		t.Fatalf("成功后应退出冷却: err=%v calls=%d", err, calls.Load())
	}

	// 404 属于业务结果，不应触发冷却。
	var notFoundCalls atomic.Int32
	p404 := NewJiuheProvider("https://mock.jiuhe.test/api", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		notFoundCalls.Add(1)
		return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"e":1}`))}, nil
	})})
	for _, id := range []string{"1", "2", "3"} {
		if _, err := p404.Detail(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("id=%s 应返回 ErrNotFound，实际 %v", id, err)
		}
	}
	if n := notFoundCalls.Load(); n != 3 {
		t.Fatalf("404 不应触发冷却，上游调用次数 = %d，应为 3", n)
	}
}

type drainTrackingBody struct {
	r      io.Reader
	n      int
	closed bool
}

func (b *drainTrackingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += n
	return n, err
}

func (b *drainTrackingBody) Close() error {
	b.closed = true
	return nil
}

// 非 200 响应体必须读空（最多 4096 字节）后再返回错误，以便连接复用。
func TestJiuheNonOKResponseBodyDrained(t *testing.T) {
	body := &drainTrackingBody{r: strings.NewReader(strings.Repeat("x", 10000))}
	p := NewJiuheProvider("https://mock.jiuhe.test/api", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: body}, nil
	})})

	if err := p.fetchJSON(context.Background(), "https://mock.jiuhe.test/api/x", &struct{}{}); err == nil {
		t.Fatal("500 响应应返回错误")
	}
	if body.n != 4096 {
		t.Fatalf("应 drain 4096 字节响应体，实际 %d", body.n)
	}
	if !body.closed {
		t.Fatal("响应体应被关闭")
	}
}

// 上游酒标地址只接受远端 http(s) 外链，站内相对路径必须清空。
func TestJiuheRejectsRelativePosterURL(t *testing.T) {
	if platform.ValidRemotePosterURL("/admin/foo") {
		t.Fatal("ValidRemotePosterURL 应拒绝 /admin/foo")
	}
	if !platform.ValidRemotePosterURL("https://cdn.example/beer.jpg") {
		t.Fatal("ValidRemotePosterURL 应接受远端 https 地址")
	}

	searchBody := `{"items":[
	  {"product":{"id":1,"name":"坏路径","coverUrl":"/admin/foo"},"wine":{"id":11,"nameZh":"坏路径","brand":{"nameZh":"厂A"}}},
	  {"product":{"id":2,"name":"站内路径","coverUrl":"/posters/evil.jpg"},"wine":{"id":12,"nameZh":"站内路径","brand":{"nameZh":"厂B"}}},
	  {"product":{"id":3,"name":"正常","coverUrl":"https://cdn.example/ok.jpg"},"wine":{"id":13,"nameZh":"正常","brand":{"nameZh":"厂C"}}}
	]}`
	detailBody := `{"product":{"product":{"id":7,"name":"详情坏图","coverUrl":"/admin/foo"}},
	  "wineDetail":{"id":17,"nameZh":"详情坏图","brandNameZh":"厂","imageUrl":"/admin/foo"}}`

	p := NewJiuheProvider("https://mock.jiuhe.test/api", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := searchBody
		if strings.Contains(r.URL.Path, "/detail") {
			body = detailBody
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})})

	got, err := p.Search(context.Background(), "ipa", 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("Search err=%v items=%d", err, len(got))
	}
	byName := map[string]string{}
	for _, c := range got {
		byName[c.Name] = c.PosterURL
	}
	if byName["坏路径"] != "" || byName["站内路径"] != "" {
		t.Fatalf("站内/相对酒标地址应被清空: %+v", byName)
	}
	if byName["正常"] != "https://cdn.example/ok.jpg" {
		t.Fatalf("合法远端酒标应保留: %+v", byName)
	}

	d, err := p.Detail(context.Background(), "7")
	if err != nil {
		t.Fatalf("Detail err=%v", err)
	}
	if d.PosterURL != "" {
		t.Fatalf("详情页站内酒标地址应被清空，实际 %q", d.PosterURL)
	}
}

type stubErrProvider struct {
	name  string
	items []Candidate
	err   error
}

func (p stubErrProvider) Name() string             { return p.name }
func (p stubErrProvider) Label() string            { return p.name }
func (p stubErrProvider) Enabled() bool            { return true }
func (p stubErrProvider) SupportsEmptyQuery() bool { return false }
func (p stubErrProvider) Search(context.Context, string, int) ([]Candidate, error) {
	return p.items, p.err
}
func (p stubErrProvider) Detail(context.Context, string) (Detail, error) {
	return Detail{}, p.err
}

func TestParseKegCapacityRateLimitCacheClockAndLookupErrors(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"30L", 30000},
		{"30L桶", 30000},
		{"500ml", 500},
		{"30la", 0},
		{"2000L", 0},
	} {
		if got := ParseKegCapacityML(tc.raw); got != tc.want {
			t.Errorf("ParseKegCapacityML(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}

	// HTTP 429 应计为瞬时失败并触发冷却熔断，第 3 次请求直接跳过上游拨号。
	var tries atomic.Int32
	jp := NewJiuheProvider("https://mock.jiuhe.test/api", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		tries.Add(1)
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("rate limit"))}, nil
	})})
	for range jiuheFailThreshold {
		if _, err := jp.Search(context.Background(), "限流", 5); err == nil {
			t.Fatal("expected 429 search error")
		}
	}
	if _, err := jp.Search(context.Background(), "限流3", 5); !errors.Is(err, errJiuheCoolingDown) {
		t.Fatalf("expected cooldown error after %d 429s, got %v", jiuheFailThreshold, err)
	}
	if tries.Load() != jiuheFailThreshold {
		t.Fatalf("expected cooldown to skip 3rd HTTP dial, tries=%d want %d", tries.Load(), jiuheFailThreshold)
	}

	// JiuheCache 注入时钟推进超过 TTL 后应失效。
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := base
	cache := &JiuheCache{
		search: make(map[string]jiuheCacheEntry[[]Candidate]),
		detail: make(map[int64]jiuheCacheEntry[Detail]),
		now:    func() time.Time { return now },
	}
	cache.setSearch("k", []Candidate{{Source: "jiuhe", Name: "缓存酒"}})
	if got, ok := cache.getSearch("k"); !ok || len(got) != 1 {
		t.Fatalf("expected immediate cache hit, got ok=%v len=%d", ok, len(got))
	}
	now = base.Add(24 * time.Hour)
	if _, ok := cache.getSearch("k"); ok {
		t.Fatal("expected cache miss after injected clock passed TTL")
	}

	// 所有检索源都失败时返回 500；仅本地失败而远端成功时返回 200 并附带 warning。
	svcAllFail := NewService(
		stubErrProvider{name: "catalog", err: errors.New("db down")},
		stubErrProvider{name: "jiuhe", err: errors.New("upstream down")},
	)
	recAll := httptest.NewRecorder()
	svcAllFail.LookupBeers(recAll, httptest.NewRequest(http.MethodGet, "/api/beers/lookup?q=ipa", nil))
	if recAll.Code != http.StatusInternalServerError {
		t.Fatalf("all providers failing should return 500, got %d", recAll.Code)
	}

	svcCatWarn := NewService(
		stubErrProvider{name: "catalog", err: errors.New("db down")},
		stubErrProvider{name: "jiuhe", items: []Candidate{{Source: "jiuhe", Name: "远端酒"}}},
	)
	recWarn := httptest.NewRecorder()
	svcCatWarn.LookupBeers(recWarn, httptest.NewRequest(http.MethodGet, "/api/beers/lookup?q=ipa", nil))
	if recWarn.Code != http.StatusOK {
		t.Fatalf("catalog fail + remote ok should return 200, got %d", recWarn.Code)
	}
	var resp Response
	if err := json.NewDecoder(recWarn.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 || resp.Warning == "" {
		t.Fatalf("expected 1 remote item and catalog warning, got %+v", resp)
	}
}
