package catalog

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"pourops/internal/platform"
)

// Codec 是历史酒单导入导出的编译期扩展接口；新增 CSV/JSON 之外的格式（如收银导出/门店模板）只需实现本接口并注册。
type Codec interface {
	Format() string
	ContentType() string
	FileExt() string
	CanDecode(filename string, trimmed []byte) bool
	Decode(trimmed []byte) ([]ImportItem, error)
	Encode(items []CatalogBeer, brand string, now time.Time) ([]byte, error)
}

// CodecRegistry 管理已注册的导入导出格式编解码器。
type CodecRegistry struct {
	codecs []Codec
}

func NewCodecRegistry(codecs ...Codec) *CodecRegistry {
	return &CodecRegistry{codecs: append([]Codec(nil), codecs...)}
}

func NewDefaultCodecRegistry() *CodecRegistry {
	return NewCodecRegistry(CSVCodec{}, JSONCodec{})
}

func (r *CodecRegistry) Register(c Codec) {
	if c != nil {
		r.codecs = append(r.codecs, c)
	}
}

func (r *CodecRegistry) Find(format string) (Codec, bool) {
	target := strings.ToLower(strings.TrimSpace(format))
	for _, c := range r.codecs {
		if strings.EqualFold(c.Format(), target) {
			return c, true
		}
	}
	return nil, false
}

func (r *CodecRegistry) Decode(filename string, raw []byte) ([]ImportItem, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("文件必须是 UTF-8 编码的 CSV 或 JSON")
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("导入文件为空")
	}
	lowerName := strings.ToLower(strings.TrimSpace(filename))
	for _, c := range r.codecs {
		if ext := strings.ToLower(c.FileExt()); ext != "" && strings.HasSuffix(lowerName, ext) {
			return c.Decode(trimmed)
		}
	}
	for _, c := range r.codecs {
		if c.CanDecode(lowerName, trimmed) {
			return c.Decode(trimmed)
		}
	}
	return nil, errors.New("不支持的导入文件格式")
}

// ---------- CSVCodec ----------

type CSVCodec struct{}

func (CSVCodec) Format() string      { return "csv" }
func (CSVCodec) ContentType() string { return "text/csv; charset=utf-8" }
func (CSVCodec) FileExt() string     { return ".csv" }
func (CSVCodec) CanDecode(filename string, trimmed []byte) bool {
	return strings.HasSuffix(filename, ".csv") || (trimmed[0] != '{' && trimmed[0] != '[')
}
func (CSVCodec) Decode(trimmed []byte) ([]ImportItem, error) { return ParseCatalogCSV(trimmed) }
func (CSVCodec) Encode(items []CatalogBeer, _ string, _ time.Time) ([]byte, error) {
	var buf bytes.Buffer
	if err := EncodeCatalogCSV(&buf, items); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------- JSONCodec ----------

type JSONCodec struct{}

func (JSONCodec) Format() string      { return "json" }
func (JSONCodec) ContentType() string { return "application/json; charset=utf-8" }
func (JSONCodec) FileExt() string     { return ".json" }
func (JSONCodec) CanDecode(filename string, trimmed []byte) bool {
	return strings.HasSuffix(filename, ".json") || trimmed[0] == '{' || trimmed[0] == '['
}
func (JSONCodec) Decode(trimmed []byte) ([]ImportItem, error) { return ParseCatalogJSON(trimmed) }
func (JSONCodec) Encode(items []CatalogBeer, brand string, now time.Time) ([]byte, error) {
	return EncodeCatalogJSON(items, brand, now)
}

// ---------- 导出：CSV（UTF-8 BOM）与 JSON ----------

var catalogCSVHeaders = []string{
	"厂牌", "酒名", "风格", "分类色", "ABV", "IBU", "UT评分", "评分人数",
	"风味词", "默认桶容量ml", "默认每桶进价元", "150ml售价元", "330ml售价元", "500ml售价元", "酒标URL",
}

func isCSVFormulaPrefix(b byte) bool {
	switch b {
	case '=', '+', '-', '@', '\t', '\r':
		return true
	default:
		return false
	}
}

func escapeCSVFormula(s string) string {
	if s != "" && isCSVFormulaPrefix(s[0]) {
		return "'" + s
	}
	return s
}

func unescapeCSVFormula(s string) string {
	if len(s) >= 2 && s[0] == '\'' && isCSVFormulaPrefix(s[1]) {
		return s[1:]
	}
	return s
}

func EncodeCatalogCSV(buf *bytes.Buffer, items []CatalogBeer) error {
	if _, err := buf.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return err
	}
	cw := csv.NewWriter(buf)
	if err := cw.Write(catalogCSVHeaders); err != nil {
		return err
	}
	for _, c := range items {
		capText := ""
		if c.CapacityML > 0 {
			capText = strconv.Itoa(c.CapacityML)
		}
		catText := c.CategoryName()
		if catText == "" && c.Style != "" {
			catText = platform.CategoryLabel(platform.ToneOf(c.Style))
		}
		row := []string{
			escapeCSVFormula(c.Brewery),
			escapeCSVFormula(c.Name),
			escapeCSVFormula(c.Style),
			catText,
			c.ABVInput(),
			c.IBUInput(),
			c.UTInput(),
			c.UTCountInput(),
			escapeCSVFormula(c.TagsText()),
			capText,
			c.CostInput(),
			c.Price150Input(),
			c.Price330Input(),
			c.Price500Input(),
			escapeCSVFormula(c.PosterURL),
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

type catalogExportDoc struct {
	Version    int                 `json:"version"`
	Brand      string              `json:"brand,omitempty"`
	ExportedAt string              `json:"exported_at"`
	Items      []catalogExportItem `json:"items"`
}

type catalogExportItem struct {
	Brewery    string   `json:"brewery"`
	Name       string   `json:"name"`
	Style      string   `json:"style"`
	Category   string   `json:"category"`
	ABV        *float64 `json:"abv"`
	IBU        *int     `json:"ibu"`
	UTRating   *float64 `json:"ut_rating"`
	UTCount    *int     `json:"ut_count"`
	Tags       []string `json:"tags"`
	CapacityML int      `json:"capacity_ml"`
	Cost       string   `json:"cost"`
	Price150   string   `json:"price_150"`
	Price330   string   `json:"price_330"`
	Price500   string   `json:"price_500"`
	PosterURL  string   `json:"poster_url"`
}

func EncodeCatalogJSON(items []CatalogBeer, brand string, now time.Time) ([]byte, error) {
	out := make([]catalogExportItem, 0, len(items))
	for _, c := range items {
		tags := c.Tags
		if tags == nil {
			tags = []string{}
		}
		cat := c.Category
		if cat == "" {
			cat = platform.ToneOf(c.Style)
		}
		capML := c.CapacityML
		if capML <= 0 {
			capML = DefaultCapacityML
		}
		out = append(out, catalogExportItem{
			Brewery:    c.Brewery,
			Name:       c.Name,
			Style:      c.Style,
			Category:   cat,
			ABV:        c.ABV,
			IBU:        c.IBU,
			UTRating:   c.UT,
			UTCount:    c.UTCount,
			Tags:       tags,
			CapacityML: capML,
			Cost:       c.CostInput(),
			Price150:   c.Price150Input(),
			Price330:   c.Price330Input(),
			Price500:   c.Price500Input(),
			PosterURL:  c.PosterURL,
		})
	}
	doc := catalogExportDoc{
		Version:    1,
		Brand:      brand,
		ExportedAt: now.In(platform.ReportZone).Format(time.RFC3339),
		Items:      out,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------- 导入：CSV 与 JSON 解析 ----------

func ParseCatalogImport(filename string, raw []byte) ([]ImportItem, error) {
	return NewDefaultCodecRegistry().Decode(filename, raw)
}

var csvHeaderAliases = map[string]string{
	"厂牌": "brewery", "酒厂": "brewery", "酿酒厂": "brewery", "品牌": "brewery", "brewery": "brewery",
	"酒名": "name", "酒款名称": "name", "酒款": "name", "名称": "name", "name": "name",
	"风格": "style", "类型": "style", "style": "style",
	"分类色": "category", "分类": "category", "颜色分类": "category", "category": "category",
	"abv": "abv", "酒精度": "abv", "酒精度(%)": "abv", "酒精度（%）": "abv", "abv(%)": "abv", "abv（%）": "abv",
	"ibu": "ibu", "苦度": "ibu", "苦度(ibu)": "ibu", "苦度（ibu）": "ibu",
	"ut评分": "ut_rating", "ut": "ut_rating", "评分": "ut_rating", "ut_rating": "ut_rating", "rating": "ut_rating",
	"评分人数": "ut_count", "ut评分人数": "ut_count", "ut_count": "ut_count", "rating_count": "ut_count",
	"风味词": "tags", "风味": "tags", "标签": "tags", "tags": "tags",
	"酒标": "poster_url", "酒标url": "poster_url", "酒标地址": "poster_url", "酒标图片": "poster_url", "酒标图片地址": "poster_url", "poster": "poster_url", "poster_url": "poster_url",
	"默认桶容量ml": "capacity_ml", "默认桶容量": "capacity_ml", "每桶容量": "capacity_ml", "每桶容量ml": "capacity_ml", "每桶容量(ml)": "capacity_ml", "每桶容量（ml）": "capacity_ml", "容量": "capacity_ml", "capacity_ml": "capacity_ml", "capacity": "capacity_ml",
	"默认每桶进价元": "cost", "每桶进价": "cost", "每桶进价(元)": "cost", "每桶进价（元）": "cost", "进价": "cost", "成本": "cost", "cost": "cost",
	"每桶进价分": "cost_cents", "cost_cents": "cost_cents",
	"150ml售价元": "price_150", "150ml价": "price_150", "150ml": "price_150", "150": "price_150", "price_150": "price_150",
	"150ml分": "price_150_cents", "price_150_cents": "price_150_cents",
	"330ml售价元": "price_330", "330ml价": "price_330", "330ml": "price_330", "330": "price_330", "price_330": "price_330",
	"330ml分": "price_330_cents", "price_330_cents": "price_330_cents",
	"500ml售价元": "price_500", "500ml价": "price_500", "500ml": "price_500", "500": "price_500", "price_500": "price_500",
	"500ml分": "price_500_cents", "price_500_cents": "price_500_cents",
}

func parseImportFloat(raw, label string, maxVal float64) (*float64, error) {
	return platform.ParseStrictFloat(raw, label, 0, maxVal, true)
}

func parseImportInt(raw, label string) (*int, error) {
	return platform.ParseStrictInt(raw, label, 0, math.MaxInt32)
}

func parseImportCentsVal(raw, label string) (int, error) {
	n, err := platform.ParseStrictInt(raw, label, 0, math.MaxInt32)
	if err != nil || n == nil {
		return 0, err
	}
	return *n, nil
}

func ParseCatalogCSV(raw []byte) ([]ImportItem, error) {
	cr := csv.NewReader(bytes.NewReader(raw))
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1
	records, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("CSV 解析失败: %w", err)
	}
	if len(records) == 0 {
		return nil, errors.New("CSV 文件为空")
	}

	colIndex := make(map[string]int, len(records[0]))
	for i, h := range records[0] {
		norm := strings.ToLower(strings.TrimSpace(h))
		if norm == "" {
			continue
		}
		canon, ok := csvHeaderAliases[norm]
		if !ok {
			continue
		}
		if _, dup := colIndex[canon]; dup {
			return nil, fmt.Errorf("CSV 表头存在重复列「%s」", strings.TrimSpace(h))
		}
		colIndex[canon] = i
	}
	if _, ok := colIndex["name"]; !ok {
		return nil, errors.New("CSV 缺少必填表头「酒名」（或 name）")
	}

	var out []ImportItem
	for i, rec := range records[1:] {
		lineNo := i + 2
		blank := true
		for _, cell := range rec {
			if strings.TrimSpace(cell) != "" {
				blank = false
				break
			}
		}
		if blank {
			continue
		}
		if len(out) >= ImportMaxRows {
			return nil, fmt.Errorf("单次最多导入 %d 款酒", ImportMaxRows)
		}

		cell := func(canon string) (string, bool) {
			idx, ok := colIndex[canon]
			if !ok || idx >= len(rec) {
				return "", false
			}
			return strings.TrimSpace(rec[idx]), true
		}

		var item ImportItem
		if v, ok := cell("brewery"); ok {
			item.Beer.Brewery = unescapeCSVFormula(v)
			item.Mask.Brewery = true
		}
		rawName, _ := cell("name")
		name := strings.TrimSpace(unescapeCSVFormula(rawName))
		if name == "" {
			return nil, fmt.Errorf("第 %d 行缺少酒名", lineNo)
		}
		item.Beer.Name = name

		if v, ok := cell("style"); ok {
			item.Beer.Style = unescapeCSVFormula(v)
			item.Mask.Style = true
		}
		if v, ok := cell("category"); ok {
			cat, err := platform.ParseCategory(v)
			if err != nil {
				return nil, fmt.Errorf("第 %d 行：%w", lineNo, err)
			}
			item.Beer.Category = cat
			item.Mask.Category = true
		}
		if v, ok := cell("abv"); ok {
			abv, err := parseImportFloat(v, "ABV", 100)
			if err != nil {
				return nil, fmt.Errorf("第 %d 行：%w", lineNo, err)
			}
			item.Beer.ABV = abv
			item.Mask.ABV = true
		}
		if v, ok := cell("ibu"); ok {
			ibu, err := parseImportInt(v, "IBU")
			if err != nil {
				return nil, fmt.Errorf("第 %d 行：%w", lineNo, err)
			}
			item.Beer.IBU = ibu
			item.Mask.IBU = true
		}
		if v, ok := cell("ut_rating"); ok {
			ut, err := parseImportFloat(v, "UT 评分", 5)
			if err != nil {
				return nil, fmt.Errorf("第 %d 行：%w", lineNo, err)
			}
			item.Beer.UT = ut
			item.Mask.UT = true
		}
		if v, ok := cell("ut_count"); ok {
			cnt, err := parseImportInt(v, "评分人数")
			if err != nil {
				return nil, fmt.Errorf("第 %d 行：%w", lineNo, err)
			}
			item.Beer.UTCount = cnt
			item.Mask.UTCount = true
		}
		if v, ok := cell("tags"); ok {
			rawTags := platform.SplitList(unescapeCSVFormula(v))
			tags := make([]string, 0, len(rawTags))
			for _, t := range rawTags {
				if t = strings.TrimSpace(unescapeCSVFormula(t)); t != "" {
					tags = append(tags, t)
				}
			}
			item.Beer.Tags = tags
			item.Mask.Tags = true
		}
		if v, ok := cell("poster_url"); ok {
			v = strings.TrimSpace(unescapeCSVFormula(v))
			if !platform.ValidPosterURL(v) {
				return nil, fmt.Errorf("第 %d 行酒标地址格式不合法", lineNo)
			}
			item.Beer.PosterURL = v
			item.Mask.PosterURL = true
		}
		if v, ok := cell("capacity_ml"); ok {
			if v != "" {
				n, err := parseImportInt(v, "默认桶容量")
				if err != nil {
					return nil, fmt.Errorf("第 %d 行：%w", lineNo, err)
				}
				if n == nil || *n <= 0 {
					return nil, fmt.Errorf("第 %d 行默认桶容量「%s」必须是大于 0 的整数", lineNo, v)
				}
				item.Beer.CapacityML = *n
				item.Mask.CapacityML = true
			}
		}
		if v, ok := cell("cost_cents"); ok {
			cents, err := parseImportCentsVal(v, "进价(分)")
			if err != nil {
				return nil, fmt.Errorf("第 %d 行：%w", lineNo, err)
			}
			item.Beer.CostCents = cents
			item.Mask.CostCents = true
		} else if v, ok := cell("cost"); ok {
			cents, err := platform.MoneyCents(v)
			if err != nil {
				return nil, fmt.Errorf("第 %d 行每桶进价：%w", lineNo, err)
			}
			item.Beer.CostCents = cents
			item.Mask.CostCents = true
		}
		for _, spec := range []struct {
			centsKey string
			yuanKey  string
			label    string
			dst      *int
			flag     *bool
		}{
			{"price_150_cents", "price_150", "150ml 售价", &item.Beer.Price150, &item.Mask.Price150},
			{"price_330_cents", "price_330", "330ml 售价", &item.Beer.Price330, &item.Mask.Price330},
			{"price_500_cents", "price_500", "500ml 售价", &item.Beer.Price500, &item.Mask.Price500},
		} {
			if v, ok := cell(spec.centsKey); ok {
				cents, err := parseImportCentsVal(v, spec.label)
				if err != nil {
					return nil, fmt.Errorf("第 %d 行：%w", lineNo, err)
				}
				*spec.dst = cents
				*spec.flag = true
			} else if v, ok := cell(spec.yuanKey); ok {
				cents, err := platform.MoneyCents(v)
				if err != nil {
					return nil, fmt.Errorf("第 %d 行%s：%w", lineNo, spec.label, err)
				}
				*spec.dst = cents
				*spec.flag = true
			}
		}

		out = append(out, item)
	}
	if len(out) == 0 {
		return nil, errors.New("CSV 中没有可导入的酒款行")
	}
	return out, nil
}

func jsonScalarString(raw json.RawMessage) (string, bool, error) {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return "", true, nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return "", false, err
		}
		return strings.TrimSpace(s), false, nil
	}
	var num json.Number
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&num); err == nil {
		return num.String(), false, nil
	}
	return "", false, errors.New("字段格式无效")
}

func ParseCatalogJSON(raw []byte) ([]ImportItem, error) {
	var rawItems []json.RawMessage
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &rawItems); err != nil {
			return nil, fmt.Errorf("JSON 解析失败: %w", err)
		}
	} else {
		var envelope struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, fmt.Errorf("JSON 解析失败: %w", err)
		}
		rawItems = envelope.Items
	}
	if len(rawItems) == 0 {
		return nil, errors.New("JSON 里没有可导入的酒款数据（items 为空）")
	}
	if len(rawItems) > ImportMaxRows {
		return nil, fmt.Errorf("单次最多导入 %d 款酒", ImportMaxRows)
	}

	out := make([]ImportItem, 0, len(rawItems))
	for i, objRaw := range rawItems {
		rowNo := i + 1
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(objRaw, &obj); err != nil {
			return nil, fmt.Errorf("第 %d 条 JSON 不是有效对象: %w", rowNo, err)
		}

		var item ImportItem
		getStr := func(keys ...string) (string, bool, bool, error) {
			for _, k := range keys {
				v, ok := obj[k]
				if !ok {
					continue
				}
				s, isNull, err := jsonScalarString(v)
				return s, isNull, true, err
			}
			return "", false, false, nil
		}

		if s, _, ok, err := getStr("brewery"); err != nil {
			return nil, fmt.Errorf("第 %d 条厂牌无效", rowNo)
		} else if ok {
			item.Beer.Brewery = s
			item.Mask.Brewery = true
		}

		name, _, ok, err := getStr("name")
		if err != nil || !ok || name == "" {
			return nil, fmt.Errorf("第 %d 条缺少酒名 name", rowNo)
		}
		item.Beer.Name = name

		if s, _, ok, err := getStr("style"); err != nil {
			return nil, fmt.Errorf("第 %d 条风格无效", rowNo)
		} else if ok {
			item.Beer.Style = s
			item.Mask.Style = true
		}

		if s, _, ok, err := getStr("category"); err != nil {
			return nil, fmt.Errorf("第 %d 条分类色无效", rowNo)
		} else if ok {
			cat, err := platform.ParseCategory(s)
			if err != nil {
				return nil, fmt.Errorf("第 %d 条：%w", rowNo, err)
			}
			item.Beer.Category = cat
			item.Mask.Category = true
		}

		if s, isNull, ok, err := getStr("abv"); err != nil {
			return nil, fmt.Errorf("第 %d 条 ABV 格式无效", rowNo)
		} else if ok {
			if !isNull {
				abv, err := parseImportFloat(s, "ABV", 100)
				if err != nil {
					return nil, fmt.Errorf("第 %d 条：%w", rowNo, err)
				}
				item.Beer.ABV = abv
			}
			item.Mask.ABV = true
		}

		if s, isNull, ok, err := getStr("ibu"); err != nil {
			return nil, fmt.Errorf("第 %d 条 IBU 格式无效", rowNo)
		} else if ok {
			if !isNull {
				ibu, err := parseImportInt(s, "IBU")
				if err != nil {
					return nil, fmt.Errorf("第 %d 条：%w", rowNo, err)
				}
				item.Beer.IBU = ibu
			}
			item.Mask.IBU = true
		}

		if s, isNull, ok, err := getStr("ut_rating", "rating"); err != nil {
			return nil, fmt.Errorf("第 %d 条 UT 评分格式无效", rowNo)
		} else if ok {
			if !isNull {
				ut, err := parseImportFloat(s, "UT 评分", 5)
				if err != nil {
					return nil, fmt.Errorf("第 %d 条：%w", rowNo, err)
				}
				item.Beer.UT = ut
			}
			item.Mask.UT = true
		}

		if s, isNull, ok, err := getStr("ut_count", "rating_count"); err != nil {
			return nil, fmt.Errorf("第 %d 条评分人数格式无效", rowNo)
		} else if ok {
			if !isNull {
				cnt, err := parseImportInt(s, "评分人数")
				if err != nil {
					return nil, fmt.Errorf("第 %d 条：%w", rowNo, err)
				}
				item.Beer.UTCount = cnt
			}
			item.Mask.UTCount = true
		}

		if rawTags, ok := obj["tags"]; ok {
			tb := bytes.TrimSpace(rawTags)
			if len(tb) == 0 || bytes.Equal(tb, []byte("null")) {
				item.Beer.Tags = []string{}
			} else if tb[0] == '[' {
				var arr []string
				if err := json.Unmarshal(tb, &arr); err != nil {
					return nil, fmt.Errorf("第 %d 条风味词数组格式无效", rowNo)
				}
				clean := make([]string, 0, len(arr))
				for _, t := range arr {
					if t = strings.TrimSpace(t); t != "" {
						clean = append(clean, t)
					}
				}
				item.Beer.Tags = clean
			} else {
				s, _, err := jsonScalarString(tb)
				if err != nil {
					return nil, fmt.Errorf("第 %d 条风味词格式无效", rowNo)
				}
				item.Beer.Tags = platform.SplitList(s)
			}
			item.Mask.Tags = true
		}

		if s, _, ok, err := getStr("poster_url", "poster"); err != nil {
			return nil, fmt.Errorf("第 %d 条酒标地址无效", rowNo)
		} else if ok {
			if !platform.ValidPosterURL(s) {
				return nil, fmt.Errorf("第 %d 条酒标地址格式不合法", rowNo)
			}
			item.Beer.PosterURL = s
			item.Mask.PosterURL = true
		}

		if s, isNull, ok, err := getStr("capacity_ml"); err != nil {
			return nil, fmt.Errorf("第 %d 条默认桶容量无效", rowNo)
		} else if ok && !isNull && s != "" {
			n, err := parseImportInt(s, "默认桶容量")
			if err != nil {
				return nil, fmt.Errorf("第 %d 条：%w", rowNo, err)
			}
			if n == nil || *n <= 0 {
				return nil, fmt.Errorf("第 %d 条默认桶容量「%s」必须是大于 0 的整数", rowNo, s)
			}
			item.Beer.CapacityML = *n
			item.Mask.CapacityML = true
		}

		if s, isNull, ok, err := getStr("cost_cents"); err != nil {
			return nil, fmt.Errorf("第 %d 条进价无效", rowNo)
		} else if ok {
			if !isNull {
				cents, err := parseImportCentsVal(s, "进价(分)")
				if err != nil {
					return nil, fmt.Errorf("第 %d 条：%w", rowNo, err)
				}
				item.Beer.CostCents = cents
			}
			item.Mask.CostCents = true
		} else if s, isNull, ok, err := getStr("cost"); err != nil {
			return nil, fmt.Errorf("第 %d 条进价无效", rowNo)
		} else if ok {
			if !isNull {
				cents, err := platform.MoneyCents(s)
				if err != nil {
					return nil, fmt.Errorf("第 %d 条进价：%w", rowNo, err)
				}
				item.Beer.CostCents = cents
			}
			item.Mask.CostCents = true
		}

		for _, spec := range []struct {
			centsKey string
			yuanKey  string
			label    string
			dst      *int
			flag     *bool
		}{
			{"price_150_cents", "price_150", "150ml 售价", &item.Beer.Price150, &item.Mask.Price150},
			{"price_330_cents", "price_330", "330ml 售价", &item.Beer.Price330, &item.Mask.Price330},
			{"price_500_cents", "price_500", "500ml 售价", &item.Beer.Price500, &item.Mask.Price500},
		} {
			if s, isNull, ok, err := getStr(spec.centsKey); err != nil {
				return nil, fmt.Errorf("第 %d 条%s无效", rowNo, spec.label)
			} else if ok {
				if !isNull {
					cents, err := parseImportCentsVal(s, spec.label)
					if err != nil {
						return nil, fmt.Errorf("第 %d 条：%w", rowNo, err)
					}
					*spec.dst = cents
				}
				*spec.flag = true
			} else if s, isNull, ok, err := getStr(spec.yuanKey); err != nil {
				return nil, fmt.Errorf("第 %d 条%s无效", rowNo, spec.label)
			} else if ok {
				if !isNull {
					cents, err := platform.MoneyCents(s)
					if err != nil {
						return nil, fmt.Errorf("第 %d 条%s：%w", rowNo, spec.label, err)
					}
					*spec.dst = cents
				}
				*spec.flag = true
			}
		}

		// 兼容 playlist.json 单价 price + size 兜底格式（优先匹配主价 330ml）
		if !item.Mask.Price150 && !item.Mask.Price330 && !item.Mask.Price500 {
			if priceStr, isNull, ok, err := getStr("price"); err == nil && ok && !isNull && priceStr != "" {
				cents, err := platform.MoneyCents(priceStr)
				if err != nil {
					return nil, fmt.Errorf("第 %d 条售价：%w", rowNo, err)
				}
				if cents > 0 {
					sizeStr, _, _, _ := getStr("size")
					switch {
					case strings.Contains(sizeStr, "330"):
						item.Beer.Price330 = cents
						item.Mask.Price330 = true
					case strings.Contains(sizeStr, "150"):
						item.Beer.Price150 = cents
						item.Mask.Price150 = true
					case strings.Contains(sizeStr, "500"):
						item.Beer.Price500 = cents
						item.Mask.Price500 = true
					default:
						item.Beer.Price330 = cents
						item.Mask.Price330 = true
					}
				}
			}
		}

		out = append(out, item)
	}
	return out, nil
}
