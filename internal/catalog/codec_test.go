package catalog

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
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
		{
			Brewery:    "高岩",
			Name:       "干型西打",
			Style:      "Dry Cider",
			Category:   "西打",
			ABV:        ptrFloat(5.5),
			CapacityML: 20000,
			CostCents:  90000,
			Price330:   4200,
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
	if !strings.Contains(string(raw), ",酒款分类,") || !strings.Contains(string(raw), ",IPA,") ||
		!strings.Contains(string(raw), ",酸小麦,") || !strings.Contains(string(raw), ",西打,") {
		t.Fatalf("CSV 应导出可读中文分类名: %s", string(raw))
	}

	parsed, err := ParseCatalogImport("catalog.csv", raw)
	if err != nil {
		t.Fatalf("ParseCatalogImport(CSV): %v", err)
	}
	if len(parsed) != 3 {
		t.Fatalf("往返数量 = %d，应为 3", len(parsed))
	}
	p0 := parsed[0].Beer
	if p0.Brewery != "酿酒狗" || p0.Name != "朋克 IPA" || p0.Category != "IPA" ||
		p0.ABV == nil || *p0.ABV != 5.2 || p0.IBU == nil || *p0.IBU != 35 ||
		p0.UT == nil || *p0.UT != 3.70 || p0.UTCount == nil || *p0.UTCount != 579855 ||
		len(p0.Tags) != 3 || p0.CapacityML != 20000 || p0.CostCents != 120000 ||
		p0.Price150 != 2800 || p0.Price330 != 4500 || p0.Price500 != 6500 ||
		!strings.Contains(p0.PosterURL, "beer-5702") {
		t.Fatalf("CSV 往返第 1 条不一致: %+v", p0)
	}
	p1 := parsed[1].Beer
	if p1.Category != "酸小麦" || p1.CostCents != 165050 || p1.IBU != nil || p1.Price150 != 0 || p1.Price330 != 8800 {
		t.Fatalf("CSV 往返第 2 条不一致: %+v", p1)
	}
	p2 := parsed[2].Beer
	if p2.Category != "西打" || p2.Price330 != 4200 {
		t.Fatalf("CSV 往返第 3 条不一致: %+v", p2)
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
		{"第 2 行未知酒款分类", "酒名,酒款分类\nA,rainbow\n", "第 2 行"},
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
			Category:   "IPA",
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
			Category:   "修道院",
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
			Category:   "世涛",
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

	customReg := NewDefaultCodecRegistryWithCategories(func() []string { return []string{"IPA", "赛松"} })
	csvCodec, ok := customReg.Find("csv")
	if !ok {
		t.Fatal("missing csv codec")
	}
	items, err := csvCodec.Decode([]byte("酒名,酒款分类\n农舍,赛松\n"))
	if err != nil || len(items) != 1 || items[0].Beer.Category != "赛松" {
		t.Fatalf("自定义分类 CSV 导入失败: items=%+v err=%v", items, err)
	}
	if _, err := csvCodec.Decode([]byte("酒名,酒款分类\n拉格酒,拉格\n")); err == nil {
		t.Fatal("未出现在自定义分类列表中的分类应导入失败")
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

func TestCatalogZIPArchiveRoundTripAndSecurity(t *testing.T) {
	store, err := NewPosterStore(filepath.Join(t.TempDir(), "posters"))
	if err != nil {
		t.Fatal(err)
	}
	u1, err := store.SaveBytes(samplePNGBytes())
	if err != nil {
		t.Fatal(err)
	}
	u2, err := store.SaveBytes(sampleJPEGBytes())
	if err != nil {
		t.Fatal(err)
	}

	items := []CatalogBeer{
		{Brewery: "酿酒狗", Name: "朋克 IPA", Style: "IPA - American", Category: "IPA", PosterURL: u1, CapacityML: 20000, CostCents: 120000, Price330: 4500},
		{Brewery: "创始者", Name: "早餐世涛", Style: "Stout", Category: "世涛", PosterURL: u1, CapacityML: 20000, CostCents: 140000, Price330: 7800},
		{Brewery: "高岩", Name: "干型西打", Style: "Dry Cider", Category: "西打", PosterURL: u2, CapacityML: 20000, CostCents: 90000, Price330: 4200},
	}

	reg := NewDefaultCodecRegistryWithOptions(nil, store)
	zipCodec, ok := reg.Find("zip")
	if !ok {
		t.Fatal("missing zip codec")
	}
	rawZip, err := zipCodec.Encode(items, "PourOps", time.Date(2026, 11, 24, 12, 0, 0, 0, platform.ReportZone))
	if err != nil {
		t.Fatalf("ZIP Encode: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(rawZip), int64(len(rawZip)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	fname1, _ := ParsePosterFilename(u1)
	fname2, _ := ParsePosterFilename(u2)
	if !names["catalog.json"] || !names["posters/"+fname1] || !names["posters/"+fname2] {
		t.Fatalf("ZIP 条目缺失: %+v", names)
	}
	// 相同海报文件只应打入一份。
	if len(zr.File) != 3 {
		t.Fatalf("ZIP 条目数 = %d，应去重为 3（1 manifest + 2 posters）", len(zr.File))
	}

	decoded, err := reg.Decode("beer-catalog.zip", rawZip)
	if err != nil {
		t.Fatalf("ZIP Decode: %v", err)
	}
	if len(decoded) != 3 {
		t.Fatalf("decoded len = %d", len(decoded))
	}
	if decoded[0].Poster == nil || !bytes.Equal(decoded[0].Poster.Data, samplePNGBytes()) {
		t.Fatalf("第 1 条解码酒标字节不对: %+v", decoded[0].Poster)
	}
	if decoded[2].Poster == nil || !bytes.Equal(decoded[2].Poster.Data, sampleJPEGBytes()) {
		t.Fatalf("第 3 条解码酒标字节不对: %+v", decoded[2].Poster)
	}

	// 支持额外包了一层目录的压缩包（例如 macOS 解压后再压缩）。
	var nestedBuf bytes.Buffer
	zw := zip.NewWriter(&nestedBuf)
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		w, _ := zw.Create("backup-folder/" + f.Name)
		_, _ = w.Write(b)
	}
	_ = zw.Close()
	nestedItems, err := reg.Decode("nested.zip", nestedBuf.Bytes())
	if err != nil || len(nestedItems) != 3 || nestedItems[0].Poster == nil {
		t.Fatalf("嵌套单层目录 ZIP 解码失败: len=%d err=%v", len(nestedItems), err)
	}

	// 安全边界：拦截 zip-slip、SVG 伪装以及缺失 catalog.json 的压缩包。
	makeBadZip := func(entries map[string][]byte) []byte {
		var b bytes.Buffer
		w := zip.NewWriter(&b)
		for k, v := range entries {
			ew, _ := w.Create(k)
			_, _ = ew.Write(v)
		}
		_ = w.Close()
		return b.Bytes()
	}
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"zip-slip", makeBadZip(map[string][]byte{"catalog.json": []byte(`{"items":[{"name":"A","poster_file":"../evil.png"}]}`), "../evil.png": samplePNGBytes()})},
		{"fake-svg", makeBadZip(map[string][]byte{"catalog.json": []byte(`{"items":[{"name":"A","poster_file":"posters/a.png"}]}`), "posters/a.png": []byte(`<svg/>`)})},
		{"no-catalog", makeBadZip(map[string][]byte{"posters/a.png": samplePNGBytes()})},
		{"not-zip", []byte("厂牌,酒名\nA,B\n")},
	} {
		if _, err := zipCodec.Decode(tc.raw); err == nil {
			t.Errorf("%s 应拒绝非法压缩包", tc.name)
		}
	}
}
