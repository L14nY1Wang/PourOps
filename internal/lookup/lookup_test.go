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
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type stubProvider struct {
	name    string
	label   string
	enabled bool
	empty   bool
	items   []Candidate
	err     error
}

func (p stubProvider) Name() string             { return p.name }
func (p stubProvider) Label() string            { return p.label }
func (p stubProvider) Enabled() bool            { return p.enabled }
func (p stubProvider) SupportsEmptyQuery() bool { return p.empty }
func (p stubProvider) Search(_ context.Context, _ string, _ int) ([]Candidate, error) {
	return p.items, p.err
}
func (p stubProvider) Detail(_ context.Context, id string) (Detail, error) {
	return Detail{Source: p.name, SourceLabel: p.label, Name: "Detail-" + id}, nil
}

func TestBeerLookupEmptyPoolAndValidation(t *testing.T) {
	svc := NewService(NewLocalCatalogProvider(nil), NewJiuheProvider("https://mock.jiuhe.test", nil))

	req := httptest.NewRequest(http.MethodGet, "/admin/beers/lookup?q=朋克", nil)
	rec := httptest.NewRecorder()
	// 仅保留本地空连接池验证
	NewService(NewLocalCatalogProvider(nil)).LookupBeers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup status = %d", rec.Code)
	}
	var res Response
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode lookup: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("空连接池应返回空列表，实际: %+v", res.Items)
	}

	for _, src := range []string{"jiuhuar", "untappd", "../evil"} {
		reqBadSrc := httptest.NewRequest(http.MethodGet, "/admin/beers/lookup/detail?source="+src+"&id=1", nil)
		recBadSrc := httptest.NewRecorder()
		svc.LookupBeerDetail(recBadSrc, reqBadSrc)
		if recBadSrc.Code != http.StatusBadRequest {
			t.Fatalf("source=%q 应返回 400，实际 %d", src, recBadSrc.Code)
		}
	}

	for _, src := range []string{"catalog", "jiuhe"} {
		for _, badID := range []string{"", "0", "-3", "abc", "../etc/passwd", "5b34507f8ba5b033628b456b"} {
			reqBadID := httptest.NewRequest(http.MethodGet, "/admin/beers/lookup/detail?source="+src+"&id="+badID, nil)
			recBadID := httptest.NewRecorder()
			svc.LookupBeerDetail(recBadID, reqBadID)
			if recBadID.Code != http.StatusBadRequest {
				t.Fatalf("source=%s id=%q 应返回 400，实际 %d", src, badID, recBadID.Code)
			}
		}
	}
}

func TestProviderAggregatorOrderAndFallbackWarning(t *testing.T) {
	svc := NewService(
		stubProvider{
			name: "catalog", label: "本店", enabled: true, empty: true,
			items: []Candidate{{Source: "catalog", ID: "1", Brewery: "A", Name: "本地酒"}},
		},
		stubProvider{
			name: "jiuhe", label: "酒核", enabled: true, empty: false,
			err: errors.New("upstream down"),
		},
	)

	rec := httptest.NewRecorder()
	svc.LookupBeers(rec, httptest.NewRequest(http.MethodGet, "/admin/beers/lookup?q=IPA", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var res Response
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Items) != 1 || res.Items[0].Name != "本地酒" || res.Warning == "" {
		t.Fatalf("应保留本地结果并附带降级告警: %+v", res)
	}
}

func TestStripBreweryPrefix(t *testing.T) {
	cases := []struct {
		brewery, raw, want string
	}{
		{"FEVER酿造", "FEVER酿造 星空领航员:Citra", "星空领航员:Citra"},
		{"费登斯", "费登斯 桃桃尼尔森贾斯帕啤酒", "桃桃尼尔森贾斯帕啤酒"},
		{"树屋", "树屋 · 尤利乌斯", "尤利乌斯"},
		{"Tree House", "tree house - Julius", "Julius"},
		{"健力士", "健力士", "健力士"},
		{"野鹅", "野鹅微醺 绯红流星", "野鹅微醺 绯红流星"},
	}
	for _, tc := range cases {
		if got := StripBreweryPrefix(tc.brewery, tc.raw); got != tc.want {
			t.Errorf("StripBreweryPrefix(%q, %q) = %q, want %q", tc.brewery, tc.raw, got, tc.want)
		}
	}
}

func TestParseKegCapacityML(t *testing.T) {
	cases := map[string]int{
		"20L":     20000,
		"30L桶装":   30000,
		"19.5L":   19500,
		"20升":     20000,
		"473ml":   473,
		"20000ML": 20000,
		"":        0,
		"未知规格":    0,
	}
	for in, want := range cases {
		if got := ParseKegCapacityML(in); got != want {
			t.Errorf("ParseKegCapacityML(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestExtractFlavorTags(t *testing.T) {
	got := ExtractFlavorTags("新英格兰IPA", "利用NE IPA多汁、柔软的酒体，将为你营造出西柚、百香果与柑橘的迷人香味。")
	if got != "西柚、百香果、柑橘" {
		t.Fatalf("ExtractFlavorTags = %q, want %q", got, "西柚、百香果、柑橘")
	}

	gotSub := ExtractFlavorTags("新英格兰帝国IPA", "带来白葡萄酒、白葡萄与油桃、甜橙果香")
	if strings.Contains(gotSub, "、葡萄") {
		t.Fatalf("不应同时包含重复子串白葡萄与葡萄: %q", gotSub)
	}

	gotFallback := ExtractFlavorTags("皮尔森", "经典酿造工艺，酒体干净。")
	if gotFallback != "皮尔森" {
		t.Fatalf("无足够风味词时应兜底中文风格，实际 %q", gotFallback)
	}
}

func TestJiuheLookupHandlersAndCache(t *testing.T) {
	var searchCalls atomic.Int32
	var draftDetailCalls atomic.Int32

	jsonResp := func(status int, body string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}

	searchPayload := `{
	  "items": [
	    {
	      "product": {"id": 6652, "name": "费登斯 桃桃尼尔森贾斯帕啤酒", "coverUrl": "https://img.example/can.jpg", "category": {"id": 4, "name": "罐装"}},
	      "price": {"min": 107.0, "display": 107.0},
	      "stock": {"availableQuantity": 3},
	      "wine": {"id": 18075, "nameZh": "费登斯 桃桃尼尔森贾斯帕啤酒", "nameEn": "Fidens Jasper with Nelson Peach", "style": "新英格兰帝国IPA", "abv": 7.9, "untappdRating": 4.41, "brand": {"id": 5, "nameZh": "费登斯", "nameEn": "Fidens Brewing Co."}}
	    },
	    {
	      "product": {"id": 9991, "name": "FEVER酿造 星空领航员:Citra 罐装", "coverUrl": "https://img.example/fever-can.jpg", "category": {"id": 4, "name": "罐装"}},
	      "stock": {"availableQuantity": 10},
	      "wine": {"id": 12838, "nameZh": "FEVER酿造 星空领航员:Citra", "nameEn": "星空领航员:Citra", "style": "新英格兰IPA", "abv": 6.7, "untappdRating": 4.08, "brand": {"id": 433, "nameZh": "FEVER酿造", "nameEn": "FEVER酿造"}}
	    },
	    {
	      "product": {"id": 3545, "name": "FEVER酿造 星空领航员:Citra", "coverUrl": "https://img.example/fever-keg.jpg", "category": {"id": 2, "name": "生啤"}},
	      "stock": {"availableQuantity": 2},
	      "wine": {"id": 12838, "nameZh": "FEVER酿造 星空领航员:Citra", "nameEn": "星空领航员:Citra", "style": "新英格兰IPA", "abv": 6.7, "untappdRating": 4.08, "brand": {"id": 433, "nameZh": "FEVER酿造", "nameEn": "FEVER酿造"}}
	    }
	  ],
	  "total": 3
	}`

	draftDetailPayload := `{
	  "product": {
	    "product": {"id": 3545, "name": "FEVER酿造 星空领航员:Citra", "coverUrl": "https://img.example/fever-keg.jpg", "category": {"id": 2, "name": "生啤"}},
	    "price": {"min": 1282.5, "display": 1282.5},
	    "skus": [{"categoryName": "生啤", "capacityLabel": "20L", "price": 1282.5, "availableQuantity": 2}]
	  },
	  "wineDetail": {
	    "id": 12838, "nameZh": "FEVER酿造 星空领航员:Citra", "nameEn": "星空领航员:Citra",
	    "brandNameZh": "FEVER酿造", "brandNameEn": "FEVER酿造",
	    "abv": 6.7, "ibu": 36, "style": "新英格兰IPA",
	    "untappdRating": 4.08, "untappdRatingCount": 35,
	    "untappdStyle": "IPA - New England / Hazy",
	    "imageUrl": "https://b6spl4rhf4xx.oss-cn-shanghai.aliyuncs.com/fever.jpg",
	    "descriptionZh": "营造出西柚、百香果与柑橘的迷人香味。"
	  }
	}`

	canDetailPayload := `{
	  "product": {
	    "product": {"id": 6652, "name": "费登斯 桃桃尼尔森贾斯帕啤酒", "coverUrl": "https://img.example/can.jpg", "category": {"id": 4, "name": "罐装"}},
	    "price": {"min": 107.0, "display": 107.0},
	    "skus": [{"categoryName": "罐装", "capacityLabel": "473ml", "price": 107.0, "availableQuantity": 3}]
	  },
	  "wineDetail": {
	    "id": 18075, "nameZh": "费登斯 桃桃尼尔森贾斯帕啤酒", "nameEn": "Fidens Jasper with Nelson Peach",
	    "brandNameZh": "费登斯", "brandNameEn": "Fidens Brewing Co.",
	    "abv": 7.9, "ibu": null, "style": "新英格兰帝国IPA",
	    "untappdRating": 4.41, "untappdRatingCount": 55,
	    "untappdStyle": "IPA - Imperial / Double New England / Hazy",
	    "imageUrl": "https://b6spl4rhf4xx.oss-cn-shanghai.aliyuncs.com/fidens.jpeg",
	    "descriptionZh": "桃子的甜美、白葡萄、熟透的油桃和甜橙香气层层铺开。"
	  }
	}`

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatalf("仅允许 GET 请求，实际 %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/mall/products":
			kw := r.URL.Query().Get("keyword")
			if kw == "fail" {
				return jsonResp(http.StatusBadGateway, `{"error":"down"}`), nil
			}
			searchCalls.Add(1)
			return jsonResp(http.StatusOK, searchPayload), nil
		case "/api/mall/products/3545/detail":
			draftDetailCalls.Add(1)
			return jsonResp(http.StatusOK, draftDetailPayload), nil
		case "/api/mall/products/6652/detail":
			return jsonResp(http.StatusOK, canDetailPayload), nil
		default:
			return jsonResp(http.StatusNotFound, `{"error":"not found"}`), nil
		}
	})}

	svc := NewService(NewJiuheProvider("https://mock.jiuhe.test/api", client))

	for range 2 {
		rec := httptest.NewRecorder()
		svc.LookupBeers(rec, httptest.NewRequest(http.MethodGet, "/admin/beers/lookup?q=IPA", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("search status = %d", rec.Code)
		}
		var res Response
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Items) != 2 || res.Warning != "" {
			t.Fatalf("应去重返回 2 条，实际: %+v", res)
		}
		if res.Items[0].ID != "3545" || !res.Items[0].IsDraft || res.Items[0].Brewery != "FEVER酿造" || res.Items[0].Name != "星空领航员:Citra" {
			t.Fatalf("生啤版应优先排第 1 且剥离前缀: %+v", res.Items[0])
		}
		if res.Items[1].ID != "6652" || res.Items[1].NameEN != "Fidens Jasper with Nelson Peach" {
			t.Fatalf("第 2 条不对: %+v", res.Items[1])
		}
	}
	if n := searchCalls.Load(); n != 1 {
		t.Fatalf("搜索结果未走缓存，上游调用次数 = %d，应为 1", n)
	}

	for range 2 {
		rec := httptest.NewRecorder()
		svc.LookupBeerDetail(rec, httptest.NewRequest(http.MethodGet, "/admin/beers/lookup/detail?source=jiuhe&id=3545", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("draft detail status = %d: %s", rec.Code, rec.Body.String())
		}
		var d Detail
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		if d.Source != "jiuhe" || d.Brewery != "FEVER酿造" || d.Name != "星空领航员:Citra" ||
			d.ABV != "6.7" || d.IBU != "36" || d.UTRating != "4.08" || d.UTCount != "35" ||
			d.Style != "IPA - New England / Hazy" || d.Category != "IPA" ||
			d.CapacityML != "20000" || d.Cost != "1282.5" || d.Tags != "西柚、百香果、柑橘" {
			t.Fatalf("生啤详情映射不对: %+v", d)
		}
	}
	if n := draftDetailCalls.Load(); n != 1 {
		t.Fatalf("详情未走缓存，上游调用次数 = %d，应为 1", n)
	}

	recCan := httptest.NewRecorder()
	svc.LookupBeerDetail(recCan, httptest.NewRequest(http.MethodGet, "/admin/beers/lookup/detail?source=jiuhe&id=6652", nil))
	if recCan.Code != http.StatusOK {
		t.Fatalf("can detail status = %d", recCan.Code)
	}
	var dCan Detail
	_ = json.Unmarshal(recCan.Body.Bytes(), &dCan)
	if dCan.CapacityML != "" || dCan.Cost != "" || dCan.IBU != "" || dCan.UTRating != "4.41" || dCan.Category != "IPA" {
		t.Fatalf("罐装详情或空 IBU 映射不对: %+v", dCan)
	}

	recFail := httptest.NewRecorder()
	svc.LookupBeers(recFail, httptest.NewRequest(http.MethodGet, "/admin/beers/lookup?q=fail", nil))
	if recFail.Code != http.StatusOK {
		t.Fatalf("fail fallback status = %d", recFail.Code)
	}
	var resFail Response
	_ = json.Unmarshal(recFail.Body.Bytes(), &resFail)
	if resFail.Warning == "" {
		t.Fatal("上游失败应返回 warning 提示")
	}

	recNotFound := httptest.NewRecorder()
	svc.LookupBeerDetail(recNotFound, httptest.NewRequest(http.MethodGet, "/admin/beers/lookup/detail?source=jiuhe&id=999999", nil))
	if recNotFound.Code != http.StatusNotFound {
		t.Fatalf("不存在的商品应返 404，实际 %d", recNotFound.Code)
	}
}

func TestJiuheProviderCustomCategoriesFn(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"product":{"product":{"id":9001,"name":"农舍厂 农舍之歌","category":{"id":2,"name":"生啤"}},"price":{"min":1100,"display":1100},"skus":[{"categoryName":"生啤","capacityLabel":"20L","price":1100,"availableQuantity":1}]},"wineDetail":{"id":9001,"nameZh":"农舍厂 农舍之歌","nameEn":"Farmhouse Song","brandNameZh":"农舍厂","abv":6.2,"style":"比利时赛松","untappdStyle":"Farmhouse Ale - Saison"}}`)),
		}, nil
	})}
	p := NewJiuheProvider("https://mock.jiuhe.test/api", client)
	p.SetCategoriesFn(func() []string { return []string{"IPA", "赛松"} })
	d, err := p.Detail(t.Context(), "9001")
	if err != nil {
		t.Fatalf("Detail err=%v", err)
	}
	if d.Category != "赛松" {
		t.Fatalf("Category = %q, want 赛松", d.Category)
	}
}
