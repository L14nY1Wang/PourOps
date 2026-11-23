package catalog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pourops/internal/platform"
)

func ptrFloat(v float64) *float64 { return &v }
func ptrInt(v int) *int           { return &v }

func TestCatalogCSVRoundTrip(t *testing.T) {
	items := []CatalogBeer{
		{
			Brewery:    "酿酒狗",
			Name:       "朋克 IPA",
			Style:      "IPA - American",
			Category:   "hop",
			ABV:        ptrFloat(5.2),
			IBU:        ptrInt(35),
			UT:         ptrFloat(3.70),
			UTCount:    ptrInt(579855),
			Tags:       []string{"美式IPA", "热带水果", "苦爽"},
			CapacityML: 20000,
			CostCents:  120000,
			Price150:   2800,
			Price330:   4500,
			Price500:   6500,
			PosterURL:  "https://assets.untappd.com/site/beer_logos_hd/beer-5702_03c14_hd.jpeg",
		},
		{
			Brewery:    "泵",
			Name:       "老贵兹",
			Style:      "Lambic - Gueuze",
			Category:   "sour",
			ABV:        ptrFloat(7.0),
			Tags:       []string{"野菌", "酸爽"},
			CapacityML: 30000,
			CostCents:  165050,
			Price330:   8800,
		},
	}

	var buf bytes.Buffer
	if err := EncodeCatalogCSV(&buf, items); err != nil {
		t.Fatalf("EncodeCatalogCSV: %v", err)
	}
	raw := buf.Bytes()
	if !bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("导出的 CSV 必须带 UTF-8 BOM 以兼容 Excel")
	}
	if !strings.Contains(string(raw), ",酒花,") || !strings.Contains(string(raw), ",酸野菌,") {
		t.Fatalf("CSV 应导出可读中文分类色: %s", string(raw))
	}

	parsed, err := ParseCatalogImport("catalog.csv", raw)
	if err != nil {
		t.Fatalf("ParseCatalogImport(CSV): %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("往返数量 = %d，应为 2", len(parsed))
	}
	p0 := parsed[0].Beer
	if p0.Brewery != "酿酒狗" || p0.Name != "朋克 IPA" || p0.Category != "hop" ||
		p0.ABV == nil || *p0.ABV != 5.2 || p0.IBU == nil || *p0.IBU != 35 ||
		p0.UT == nil || *p0.UT != 3.70 || p0.UTCount == nil || *p0.UTCount != 579855 ||
		len(p0.Tags) != 3 || p0.CapacityML != 20000 || p0.CostCents != 120000 ||
		p0.Price150 != 2800 || p0.Price330 != 4500 || p0.Price500 != 6500 ||
		!strings.Contains(p0.PosterURL, "beer-5702") {
		t.Fatalf("CSV 往返第 1 条不一致: %+v", p0)
	}
	p1 := parsed[1].Beer
	if p1.Category != "sour" || p1.CostCents != 165050 || p1.IBU != nil || p1.Price150 != 0 || p1.Price330 != 8800 {
		t.Fatalf("CSV 往返第 2 条不一致: %+v", p1)
	}
}

func TestCatalogCSVPartialColumnsAndValidation(t *testing.T) {
	csvPartial := "brewery,name,abv,cost,price_330\nTree House,Julius,6.8%,¥1350,¥ 68\n"
	items, err := ParseCatalogImport("partial.csv", []byte(csvPartial))
	if err != nil {
		t.Fatalf("部分列 CSV 失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len = %d", len(items))
	}
	it := items[0]
	if it.Beer.Brewery != "Tree House" || it.Beer.Name != "Julius" ||
		it.Beer.ABV == nil || *it.Beer.ABV != 6.8 ||
		it.Beer.CostCents != 135000 || it.Beer.Price330 != 6800 {
		t.Fatalf("解析结果不对: %+v", it.Beer)
	}
	if !it.Mask.Brewery || !it.Mask.ABV || !it.Mask.CostCents || !it.Mask.Price330 {
		t.Fatalf("存在的列应标记 Present: %+v", it.Mask)
	}
	if it.Mask.Style || it.Mask.PosterURL || it.Mask.Tags || it.Mask.Price150 {
		t.Fatalf("未出现的列不应标记 Present: %+v", it.Mask)
	}

	badCases := []struct {
		name string
		body string
		want string
	}{
		{"缺少酒名表头", "厂牌,风格\n树屋,IPA\n", "酒名"},
		{"第 2 行空酒名", "厂牌,酒名\n树屋,\n", "第 2 行"},
		{"第 2 行坏 ABV", "酒名,ABV\n尤利乌斯,abc\n", "第 2 行"},
		{"第 2 行 ABV 超 100", "酒名,ABV\n尤利乌斯,101\n", "第 2 行"},
		{"第 2 行 UT 超 5", "酒名,UT评分\n尤利乌斯,5.1\n", "第 2 行"},
		{"第 2 行 IBU 超 int4", "酒名,IBU\n尤利乌斯,2147483648\n", "第 2 行"},
		{"第 2 行容量超 int4", "酒名,默认桶容量ml\n尤利乌斯,2147483648\n", "第 2 行"},
		{"第 3 行负进价", "酒名,每桶进价\nA,1000\nB,-50\n", "第 3 行"},
		{"第 2 行进价溢出", "酒名,每桶进价\nA,30000000\n", "第 2 行"},
		{"第 2 行未知分类色", "酒名,分类色\nA,rainbow\n", "第 2 行"},
		{"第 2 行危险酒标协议", "酒名,酒标URL\nA,javascript:alert(1)\n", "第 2 行"},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseCatalogImport("bad.csv", []byte(tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望包含 %q 的错误，实际: %v", tc.want, err)
			}
		})
	}

	shortCSV := "厂牌,酒名,风格,330ml售价元\n树屋,尤利乌斯\n酿酒狗,朋克IPA,IPA - American,45\n"
	shortItems, err := ParseCatalogImport("short.csv", []byte(shortCSV))
	if err != nil {
		t.Fatalf("短行 CSV 应兼容解析，实际报错: %v", err)
	}
	if len(shortItems) != 2 || shortItems[0].Beer.Name != "尤利乌斯" || shortItems[0].Mask.Style || shortItems[0].Mask.Price330 {
		t.Fatalf("短行 CSV 第 1 条解析不对: %+v", shortItems[0])
	}
	if shortItems[1].Beer.Price330 != 4500 || !shortItems[1].Mask.Style {
		t.Fatalf("短行 CSV 第 2 条解析不对: %+v", shortItems[1])
	}
}

func TestCatalogCSVFormulaInjectionAndQuotes(t *testing.T) {
	for _, tc := range []struct {
		raw, escaped string
	}{
		{"=CMD()", "'=CMD()"},
		{"+1+1", "'+1+1"},
		{"-2+3", "'-2+3"},
		{"@SUM(1,2)", "'@SUM(1,2)"},
		{"\tcmd", "'\tcmd"},
		{"\rcmd", "'\rcmd"},
		{"'t IJ", "'t IJ"},
		{"L'Chaim", "L'Chaim"},
		{"", ""},
	} {
		if got := escapeCSVFormula(tc.raw); got != tc.escaped {
			t.Errorf("escapeCSVFormula(%q) = %q, want %q", tc.raw, got, tc.escaped)
		}
		if got := unescapeCSVFormula(tc.escaped); got != tc.raw {
			t.Errorf("unescapeCSVFormula(%q) = %q, want %q", tc.escaped, got, tc.raw)
		}
	}
	if got := unescapeCSVFormula("'t IJ"); got != "'t IJ" {
		t.Errorf("正常单引号不应被剥离: %q", got)
	}

	items := []CatalogBeer{
		{
			Brewery:    "=HYPERLINK(\"http://evil\")",
			Name:       "+危险酒名",
			Style:      "-IPA",
			Category:   "hop",
			Tags:       []string{"@果香", "+苦韵", "'t IJ"},
			CapacityML: 20000,
			CostCents:  120000,
			Price330:   4500,
			PosterURL:  "https://assets.untappd.com/ok.jpg",
		},
		{
			Brewery:    "'t IJ",
			Name:       "'t IJ Dubbel",
			Style:      "Dubbel",
			Category:   "dark",
			Tags:       []string{"'t IJ 麦香"},
			CapacityML: 20000,
		},
	}

	var buf bytes.Buffer
	if err := EncodeCatalogCSV(&buf, items); err != nil {
		t.Fatalf("EncodeCatalogCSV: %v", err)
	}
	csvText := buf.String()
	for _, wantEsc := range []string{"'=HYPERLINK", "'+危险酒名", "'-IPA", "'@果香"} {
		if !strings.Contains(csvText, wantEsc) {
			t.Fatalf("CSV 导出未对公式前缀转义 %q:\n%s", wantEsc, csvText)
		}
	}
	if strings.Contains(csvText, "''t IJ") {
		t.Fatalf("正常单引号不应被重复加前缀:\n%s", csvText)
	}

	parsed, err := ParseCatalogImport("formula.csv", buf.Bytes())
	if err != nil {
		t.Fatalf("往返导入失败: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("数量 = %d", len(parsed))
	}
	p0 := parsed[0].Beer
	if p0.Brewery != items[0].Brewery || p0.Name != items[0].Name || p0.Style != items[0].Style ||
		len(p0.Tags) != 3 || p0.Tags[0] != "@果香" || p0.Tags[1] != "+苦韵" || p0.Tags[2] != "'t IJ" {
		t.Fatalf("公式转义往返失真: %+v", p0)
	}
	p1 := parsed[1].Beer
	if p1.Brewery != "'t IJ" || p1.Name != "'t IJ Dubbel" || len(p1.Tags) != 1 || p1.Tags[0] != "'t IJ 麦香" {
		t.Fatalf("正常单引号往返失真: %+v", p1)
	}

	manualTagsCSV := "酒名,风味词\n测试,\"'+柑橘、'=果香、't IJ\"\n"
	tagParsed, err := ParseCatalogImport("tags.csv", []byte(manualTagsCSV))
	if err != nil {
		t.Fatalf("手工转义风味词解析失败: %v", err)
	}
	gotTags := tagParsed[0].Beer.Tags
	if len(gotTags) != 3 || gotTags[0] != "+柑橘" || gotTags[1] != "=果香" || gotTags[2] != "'t IJ" {
		t.Fatalf("风味词分段还原不对: %#v", gotTags)
	}
}

func TestCatalogJSONRoundTripAndPlaylistCompat(t *testing.T) {
	items := []CatalogBeer{
		{
			Brewery:    "创始者",
			Name:       "早餐世涛",
			Style:      "Stout - Imperial Coffee",
			Category:   "dark",
			ABV:        ptrFloat(8.3),
			IBU:        ptrInt(60),
			UT:         ptrFloat(4.11),
			UTCount:    ptrInt(374305),
			Tags:       []string{"咖啡", "巧克力", "燕麦"},
			CapacityML: 20000,
			CostCents:  140000,
			Price150:   3800,
			Price330:   7800,
			PosterURL:  "https://assets.untappd.com/site/beer_logos_hd/beer-4589_5470f_hd.jpeg",
		},
	}

	raw, err := EncodeCatalogJSON(items, "PourOps", time.Date(2026, 11, 23, 12, 0, 0, 0, platform.ReportZone))
	if err != nil {
		t.Fatalf("EncodeCatalogJSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("JSON 不可解析: %v", err)
	}
	if doc["brand"] != "PourOps" {
		t.Fatalf("brand = %v", doc["brand"])
	}

	parsed, err := ParseCatalogImport("export.json", raw)
	if err != nil {
		t.Fatalf("ParseCatalogImport(JSON): %v", err)
	}
	if len(parsed) != 1 || parsed[0].Beer.CostCents != 140000 || parsed[0].Beer.Price330 != 7800 || len(parsed[0].Beer.Tags) != 3 {
		t.Fatalf("JSON 往返不一致: %+v", parsed)
	}

	playlistBytes, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "screen", "playlist.json"))
	if err != nil {
		t.Fatalf("读取 web/static/screen/playlist.json 失败: %v", err)
	}
	plItems, err := ParseCatalogImport("playlist.json", playlistBytes)
	if err != nil {
		t.Fatalf("解析 web/static/screen/playlist.json 失败: %v", err)
	}
	if len(plItems) != 12 {
		t.Fatalf("playlist.json 条数 = %d，应为 12", len(plItems))
	}
	first := plItems[0].Beer
	if first.Brewery != "酿酒狗" || first.Name != "朋克 IPA" || first.Price330 != 4500 ||
		!strings.Contains(first.PosterURL, "assets.untappd.com") {
		t.Fatalf("playlist.json 第一款解析不对: %+v", first)
	}
	last := plItems[11].Beer
	if last.Name != "小麦白啤" || last.Price500 != 5800 || last.Price330 != 0 {
		t.Fatalf("playlist.json 500ml 价格映射不对: %+v", last)
	}

	mixedPlaylist := []byte(`{"items":[
		{"name":"混合杯型","size":"150/330ml","price":"¥ 45"},
		{"name":"三杯型","size":"150/330/500ml","price":"¥ 58"},
		{"name":"仅小杯","size":"150ml","price":"¥ 28"}
	]}`)
	mixedItems, err := ParseCatalogImport("playlist-mixed.json", mixedPlaylist)
	if err != nil {
		t.Fatalf("混合杯型 playlist 解析失败: %v", err)
	}
	if mixedItems[0].Beer.Price330 != 4500 || mixedItems[0].Beer.Price150 != 0 {
		t.Fatalf("150/330ml 应映射到 Price330，实际: %+v", mixedItems[0].Beer)
	}
	if mixedItems[1].Beer.Price330 != 5800 || mixedItems[1].Beer.Price150 != 0 || mixedItems[1].Beer.Price500 != 0 {
		t.Fatalf("150/330/500ml 应映射到 Price330，实际: %+v", mixedItems[1].Beer)
	}
	if mixedItems[2].Beer.Price150 != 2800 || mixedItems[2].Beer.Price330 != 0 {
		t.Fatalf("150ml 应映射到 Price150，实际: %+v", mixedItems[2].Beer)
	}
}

func TestCodecRegistryLookupAndCustomCodec(t *testing.T) {
	reg := NewDefaultCodecRegistry()
	if c, ok := reg.Find("CSV"); !ok || c.ContentType() != "text/csv; charset=utf-8" {
		t.Fatalf("Find(CSV) = %v, %v", c, ok)
	}
	if _, ok := reg.Find("xml"); ok {
		t.Fatal("未知格式应返回 false")
	}
}

func TestValidPosterURL(t *testing.T) {
	for _, okURL := range []string{
		"",
		"https://assets.untappd.com/site/beer_logos_hd/beer-5702_03c14_hd.jpeg",
		"http://pourops.example:8080/posters/test.png",
		"/static/brand/pourops.svg",
	} {
		if !platform.ValidPosterURL(okURL) {
			t.Errorf("合法 URL %q 不应被拒", okURL)
		}
	}
	for _, badURL := range []string{
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"//evil.example.com/a.png",
		"ftp://example.com/a.png",
		"https://",
		"not-a-url",
	} {
		if platform.ValidPosterURL(badURL) {
			t.Errorf("危险或非法 URL %q 应被拦截", badURL)
		}
	}
}
