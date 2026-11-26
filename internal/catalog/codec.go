package catalog

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"pourops/internal/platform"
)

const (
	maxZipEntries           = ImportMaxRows + 1
	maxZipTotalUncompressed = 80 << 20
	zipCatalogFilename      = "catalog.json"
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
	return NewDefaultCodecRegistryWithCategories(nil)
}

// NewDefaultCodecRegistryWithCategories 使用动态酒款分类表构建默认 ZIP/CSV/JSON 编解码注册表。
func NewDefaultCodecRegistryWithCategories(categoriesFn func() []string) *CodecRegistry {
	return NewDefaultCodecRegistryWithOptions(categoriesFn, nil)
}

// NewDefaultCodecRegistryWithOptions 使用动态酒款分类表和本地酒标存储构建默认导入导出编解码注册表。
func NewDefaultCodecRegistryWithOptions(categoriesFn func() []string, posters *PosterStore) *CodecRegistry {
	return NewCodecRegistry(
		ZIPCodec{CategoriesFn: categoriesFn, Posters: posters},
		CSVCodec{CategoriesFn: categoriesFn},
		JSONCodec{CategoriesFn: categoriesFn},
	)
}

func resolveCodecCategories(fn func() []string) []string {
	if fn != nil {
		if cats := fn(); len(cats) > 0 {
			return cats
		}
	}
	return platform.DefaultBeerCategories
}

func hasZIPMagic(raw []byte) bool {
	return len(raw) >= 4 && ((raw[0] == 'P' && raw[1] == 'K' && raw[2] == 3 && raw[3] == 4) ||
		(raw[0] == 'P' && raw[1] == 'K' && raw[2] == 5 && raw[3] == 6))
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
	if len(raw) == 0 {
		return nil, errors.New("导入文件为空")
	}
	lowerName := strings.ToLower(strings.TrimSpace(filename))
	for _, c := range r.codecs {
		if ext := strings.ToLower(c.FileExt()); ext != "" && strings.HasSuffix(lowerName, ext) {
			return c.Decode(raw)
		}
	}
	for _, c := range r.codecs {
		if c.CanDecode(lowerName, raw) {
			return c.Decode(raw)
		}
	}
	return nil, errors.New("不支持的导入文件格式")
}

// ---------- ZIPCodec ----------

type ZIPCodec struct {
	CategoriesFn func() []string
	Posters      *PosterStore
}

func (ZIPCodec) Format() string      { return "zip" }
func (ZIPCodec) ContentType() string { return "application/zip" }
func (ZIPCodec) FileExt() string     { return ".zip" }
func (ZIPCodec) CanDecode(filename string, raw []byte) bool {
	return strings.HasSuffix(filename, ".zip") || hasZIPMagic(raw)
}

type zipPosterAsset struct {
	rel       string
	posterURL string
}

// PreparedZIPArchive 保存已完成边界预检且可直接流式输出的历史酒单压缩包清单。
type PreparedZIPArchive struct {
	posters   *PosterStore
	assets    []zipPosterAsset
	jsonBytes []byte
	now       time.Time
}

func (p *PreparedZIPArchive) WriteTo(w io.Writer) error {
	zw := zip.NewWriter(w)
	for _, asset := range p.assets {
		_, f, _, ok := p.posters.Open(asset.posterURL)
		if !ok {
			return fmt.Errorf("导出酒标文件缺失: %s", asset.rel)
		}
		fh := &zip.FileHeader{
			Name:     asset.rel,
			Method:   zip.Store,
			Modified: p.now,
		}
		fw, err := zw.CreateHeader(fh)
		if err != nil {
			_ = f.Close()
			return err
		}
		_, err = io.Copy(fw, io.LimitReader(f, MaxPosterBytes+1))
		_ = f.Close()
		if err != nil {
			return err
		}
	}

	mfh := &zip.FileHeader{
		Name:     zipCatalogFilename,
		Method:   zip.Deflate,
		Modified: p.now,
	}
	mw, err := zw.CreateHeader(mfh)
	if err != nil {
		return err
	}
	if _, err := mw.Write(p.jsonBytes); err != nil {
		return err
	}
	return zw.Close()
}

// PrepareExport 在写入任何响应字节前校验条数、条目数与压缩包体积上限，确保导出的 ZIP 可再导回。
func (c ZIPCodec) PrepareExport(items []CatalogBeer, brand string, now time.Time) (*PreparedZIPArchive, error) {
	if len(items) > ImportMaxRows {
		return nil, platform.UserErrf("导出酒款超过单包上限（%d 款），请精简后再导出", ImportMaxRows)
	}
	exportItems := append([]CatalogBeer(nil), items...)
	posterFiles := make([]string, len(exportItems))
	writtenPosters := make(map[string]bool)
	var assets []zipPosterAsset
	var totalBytes int64

	for i := range exportItems {
		pURL := strings.TrimSpace(exportItems[i].PosterURL)
		if c.Posters != nil && IsLocalPosterURL(pURL) {
			if name, size, ok := c.Posters.FileInfo(pURL); ok {
				rel := "posters/" + name
				if !writtenPosters[rel] {
					writtenPosters[rel] = true
					assets = append(assets, zipPosterAsset{rel: rel, posterURL: pURL})
					totalBytes += size
				}
				posterFiles[i] = rel
			} else {
				exportItems[i].PosterURL = ""
			}
		}
	}

	entryCount := 1 + len(assets)
	if entryCount > maxZipEntries {
		return nil, platform.UserErrf("导出文件条目数超过上限（%d 个）", maxZipEntries)
	}

	jsonBytes, err := encodeCatalogJSONWithPosterFiles(exportItems, posterFiles, brand, now, resolveCodecCategories(c.CategoriesFn), 2)
	if err != nil {
		return nil, err
	}
	if len(jsonBytes) > ImportMaxBytes {
		return nil, platform.UserErr("导出清单超出单文件大小上限，请精简后再导出")
	}
	totalBytes += int64(len(jsonBytes))
	if totalBytes+int64(len(assets)+1)*256+4096 > ImportArchiveMaxBytes {
		return nil, platform.UserErrf("导出压缩包体积超过导入上限（50MB），请减少单次酒标后重试")
	}

	return &PreparedZIPArchive{
		posters:   c.Posters,
		assets:    assets,
		jsonBytes: jsonBytes,
		now:       now,
	}, nil
}

func (c ZIPCodec) EncodeTo(w io.Writer, items []CatalogBeer, brand string, now time.Time) error {
	prepared, err := c.PrepareExport(items, brand, now)
	if err != nil {
		return err
	}
	return prepared.WriteTo(w)
}

func (c ZIPCodec) Encode(items []CatalogBeer, brand string, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	if err := c.EncodeTo(&buf, items, brand, now); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func normalizeZipEntryPath(raw string) (string, bool) {
	if raw == "" || strings.Contains(raw, "\\") || strings.HasPrefix(raw, "/") {
		return "", false
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	parts := strings.Split(raw, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", false
		}
	}
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", false
	}
	return clean, true
}

func readZipEntryBounded(f *zip.File, maxEntryBytes int64, totalLeft *int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(maxEntryBytes) {
		return nil, fmt.Errorf("压缩包内文件 %s 体积超过上限", f.Name)
	}
	limit := maxEntryBytes
	if *totalLeft < limit {
		limit = *totalLeft
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("无法读取压缩包内文件 %s: %w", f.Name, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("读取压缩包内文件 %s 失败: %w", f.Name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("压缩包解压体积超过上限（文件 %s）", f.Name)
	}
	*totalLeft -= int64(len(data))
	return data, nil
}

type zipPosterRef struct {
	PosterFile string `json:"poster_file"`
	PosterURL  string `json:"poster_url"`
	Poster     string `json:"poster"`
}

func extractZipPosterRefs(trimmedJSON []byte) ([]zipPosterRef, error) {
	if len(trimmedJSON) == 0 {
		return nil, nil
	}
	if trimmedJSON[0] == '[' {
		var refs []zipPosterRef
		if err := json.Unmarshal(trimmedJSON, &refs); err != nil {
			return nil, err
		}
		return refs, nil
	}
	var env struct {
		Items []zipPosterRef `json:"items"`
	}
	if err := json.Unmarshal(trimmedJSON, &env); err != nil {
		return nil, err
	}
	return env.Items, nil
}

func (c ZIPCodec) Decode(raw []byte) ([]ImportItem, error) {
	if !hasZIPMagic(raw) {
		return nil, errors.New("请上传本系统导出的 .zip 历史酒单压缩包")
	}
	return c.DecodeReaderAt(bytes.NewReader(raw), int64(len(raw)))
}

// DecodeReaderAt 直接从随机读源流式解析 ZIP（如临时上传文件），避免整包读入内存。
func (c ZIPCodec) DecodeReaderAt(r io.ReaderAt, size int64) ([]ImportItem, error) {
	if r == nil || size < 4 {
		return nil, errors.New("请上传本系统导出的 .zip 历史酒单压缩包")
	}
	var head [4]byte
	if n, err := r.ReadAt(head[:], 0); n < 4 || (err != nil && !errors.Is(err, io.EOF)) || !hasZIPMagic(head[:n]) {
		return nil, errors.New("请上传本系统导出的 .zip 历史酒单压缩包")
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, errors.New("压缩包损坏或格式不支持")
	}
	if len(zr.File) == 0 {
		return nil, errors.New("压缩包为空")
	}
	if len(zr.File) > maxZipEntries {
		return nil, fmt.Errorf("压缩包内文件数量过多（上限 %d）", maxZipEntries)
	}

	entries := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") || f.FileInfo().IsDir() {
			continue
		}
		if strings.HasPrefix(f.Name, "__MACOSX/") || path.Base(f.Name) == ".DS_Store" {
			continue
		}
		norm, ok := normalizeZipEntryPath(f.Name)
		if !ok {
			return nil, fmt.Errorf("压缩包包含不安全路径 %q", f.Name)
		}
		entries[norm] = f
	}
	if len(entries) == 0 {
		return nil, errors.New("压缩包内没有有效文件")
	}

	// 支持根目录 catalog.json 或整体多套一层同名目录（如 beer-catalog/catalog.json）。
	if _, hasRoot := entries[zipCatalogFilename]; !hasRoot {
		var commonTop string
		for p := range entries {
			top, _, hasSlash := strings.Cut(p, "/")
			if !hasSlash || top == "" {
				commonTop = ""
				break
			}
			if commonTop == "" {
				commonTop = top
			} else if commonTop != top {
				commonTop = ""
				break
			}
		}
		if commonTop != "" {
			prefix := commonTop + "/"
			if _, ok := entries[prefix+zipCatalogFilename]; ok {
				stripped := make(map[string]*zip.File, len(entries))
				for k, v := range entries {
					stripped[strings.TrimPrefix(k, prefix)] = v
				}
				entries = stripped
			}
		}
	}

	catEntry, ok := entries[zipCatalogFilename]
	if !ok {
		return nil, errors.New("压缩包缺少 catalog.json 文件")
	}

	totalLeft := int64(maxZipTotalUncompressed)
	catRaw, err := readZipEntryBounded(catEntry, ImportMaxBytes, &totalLeft)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(catRaw) {
		return nil, errors.New("catalog.json 必须是 UTF-8 编码")
	}
	catTrimmed := bytes.TrimSpace(bytes.TrimPrefix(catRaw, []byte{0xEF, 0xBB, 0xBF}))
	items, err := ParseCatalogJSONWithCategories(catTrimmed, resolveCodecCategories(c.CategoriesFn))
	if err != nil {
		return nil, err
	}
	refs, err := extractZipPosterRefs(catTrimmed)
	if err != nil || len(refs) != len(items) {
		return nil, errors.New("解析 catalog.json 中的酒标索引失败")
	}

	posterCache := make(map[string][]byte)
	for i := range items {
		rowNo := i + 1
		refPath := strings.TrimSpace(refs[i].PosterFile)
		explicitRef := refPath != ""
		if !explicitRef {
			pURL := strings.TrimSpace(refs[i].PosterURL)
			if pURL == "" {
				pURL = strings.TrimSpace(refs[i].Poster)
			}
			if name, ok := ParsePosterFilename(pURL); ok {
				refPath = "posters/" + name
			}
		}
		if refPath == "" {
			continue
		}
		normPoster, ok := normalizeZipEntryPath(refPath)
		if !ok {
			return nil, fmt.Errorf("第 %d 条酒标的压缩包路径 %q 不合法", rowNo, refPath)
		}
		imgBytes, cached := posterCache[normPoster]
		if !cached {
			pf, exists := entries[normPoster]
			if !exists {
				if explicitRef {
					return nil, fmt.Errorf("第 %d 条引用的酒标文件 %q 在压缩包中不存在", rowNo, normPoster)
				}
				continue
			}
			imgBytes, err = readZipEntryBounded(pf, MaxPosterBytes, &totalLeft)
			if err != nil {
				return nil, fmt.Errorf("第 %d 条酒标：%w", rowNo, err)
			}
			if _, _, ok := DetectPosterExt(imgBytes); !ok {
				return nil, fmt.Errorf("第 %d 条酒标文件 %q 不是合法的 JPG/PNG/WebP/GIF 图片", rowNo, normPoster)
			}
			posterCache[normPoster] = imgBytes
		}
		items[i].Poster = &PosterPayload{
			Filename: path.Base(normPoster),
			Data:     imgBytes,
		}
		items[i].Mask.PosterURL = true
	}
	return items, nil
}

// ---------- CSVCodec ----------

type CSVCodec struct {
	CategoriesFn func() []string
}

func (CSVCodec) Format() string      { return "csv" }
func (CSVCodec) ContentType() string { return "text/csv; charset=utf-8" }
func (CSVCodec) FileExt() string     { return ".csv" }
func (CSVCodec) CanDecode(filename string, raw []byte) bool {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}))
	return strings.HasSuffix(filename, ".csv") || (!hasZIPMagic(raw) && len(trimmed) > 0 && trimmed[0] != '{' && trimmed[0] != '[' && utf8.Valid(raw))
}
func (c CSVCodec) Decode(raw []byte) ([]ImportItem, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("文件必须是 UTF-8 编码的 CSV")
	}
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}))
	if len(trimmed) == 0 {
		return nil, errors.New("CSV 文件为空")
	}
	return ParseCatalogCSVWithCategories(trimmed, resolveCodecCategories(c.CategoriesFn))
}
func (c CSVCodec) Encode(items []CatalogBeer, _ string, _ time.Time) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeCatalogCSVWithCategories(&buf, items, resolveCodecCategories(c.CategoriesFn)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------- JSONCodec ----------

type JSONCodec struct {
	CategoriesFn func() []string
}

func (JSONCodec) Format() string      { return "json" }
func (JSONCodec) ContentType() string { return "application/json; charset=utf-8" }
func (JSONCodec) FileExt() string     { return ".json" }
func (JSONCodec) CanDecode(filename string, raw []byte) bool {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}))
	return strings.HasSuffix(filename, ".json") || (len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '['))
}
func (c JSONCodec) Decode(raw []byte) ([]ImportItem, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("文件必须是 UTF-8 编码的 JSON")
	}
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}))
	if len(trimmed) == 0 {
		return nil, errors.New("JSON 文件为空")
	}
	return ParseCatalogJSONWithCategories(trimmed, resolveCodecCategories(c.CategoriesFn))
}
func (c JSONCodec) Encode(items []CatalogBeer, brand string, now time.Time) ([]byte, error) {
	return encodeCatalogJSONWithCategories(items, brand, now, resolveCodecCategories(c.CategoriesFn))
}

// ---------- 导出：CSV（UTF-8 BOM）与 JSON ----------

var catalogCSVHeaders = []string{
	"厂牌", "酒名", "风格", "酒款分类", "ABV", "IBU", "UT评分", "评分人数",
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
	return encodeCatalogCSVWithCategories(buf, items, platform.DefaultBeerCategories)
}

func encodeCatalogCSVWithCategories(buf *bytes.Buffer, items []CatalogBeer, cats []string) error {
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
			catText = platform.InferCategory(c.Style, cats)
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
	PosterFile string   `json:"poster_file,omitempty"`
}

func EncodeCatalogJSON(items []CatalogBeer, brand string, now time.Time) ([]byte, error) {
	return encodeCatalogJSONWithCategories(items, brand, now, platform.DefaultBeerCategories)
}

func encodeCatalogJSONWithCategories(items []CatalogBeer, brand string, now time.Time, cats []string) ([]byte, error) {
	return encodeCatalogJSONWithPosterFiles(items, nil, brand, now, cats, 1)
}

func encodeCatalogJSONWithPosterFiles(items []CatalogBeer, posterFiles []string, brand string, now time.Time, cats []string, version int) ([]byte, error) {
	out := make([]catalogExportItem, 0, len(items))
	for i, c := range items {
		tags := c.Tags
		if tags == nil {
			tags = []string{}
		}
		cat := platform.CategoryLabel(c.Category)
		if cat == "" {
			cat = platform.InferCategory(c.Style, cats)
		}
		capML := c.CapacityML
		if capML <= 0 {
			capML = DefaultCapacityML
		}
		posterFile := ""
		if i < len(posterFiles) {
			posterFile = posterFiles[i]
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
			PosterFile: posterFile,
		})
	}
	doc := catalogExportDoc{
		Version:    version,
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
	"酒款分类": "category", "分类色": "category", "分类": "category", "颜色分类": "category", "category": "category",
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

func normalizeDecodeCategory(raw string, allowed []string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if strings.ContainsAny(v, "\r\n\x00") || len([]rune(v)) > 50 {
		return "", platform.UserErr("酒款分类格式不合法")
	}
	if matched, err := platform.ParseCategoryWithAllowed(v, allowed); err == nil {
		return matched, nil
	}
	// P2-17：解码阶段仅做兼容转换与格式检查，不因已删自定义分类拒绝导入备份。
	return platform.CategoryLabel(v), nil
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
	return ParseCatalogCSVWithCategories(raw, platform.DefaultBeerCategories)
}

func ParseCatalogCSVWithCategories(raw []byte, allowed []string) ([]ImportItem, error) {
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
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
			cat, err := normalizeDecodeCategory(v, allowed)
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
		if err := platform.ValidateBeerTexts(item.Beer.Brewery, item.Beer.Name, item.Beer.Style,
			item.Beer.Category, item.Beer.PosterURL, item.Beer.Tags); err != nil {
			return nil, fmt.Errorf("第 %d 行：%w", lineNo, err)
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
	return ParseCatalogJSONWithCategories(raw, platform.DefaultBeerCategories)
}

func ParseCatalogJSONWithCategories(raw []byte, allowed []string) ([]ImportItem, error) {
	raw = bytes.TrimSpace(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}))
	if len(raw) == 0 {
		return nil, errors.New("JSON 文件为空")
	}
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
			return nil, fmt.Errorf("第 %d 条酒款分类无效", rowNo)
		} else if ok {
			cat, err := normalizeDecodeCategory(s, allowed)
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
		if err := platform.ValidateBeerTexts(item.Beer.Brewery, item.Beer.Name, item.Beer.Style,
			item.Beer.Category, item.Beer.PosterURL, item.Beer.Tags); err != nil {
			return nil, fmt.Errorf("第 %d 条：%w", rowNo, err)
		}

		out = append(out, item)
	}
	return out, nil
}
