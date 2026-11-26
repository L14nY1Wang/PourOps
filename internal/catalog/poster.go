package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
)

const (
	PosterURLPrefix    = "/posters/"
	MaxPosterBytes     = 5 << 20
	posterFetchTimeout = 15 * time.Second
	maxPosterRedirects = 5

	// 酒标分辨率上限：单边 4096 像素且总像素不超过 16777216（4096×4096），用于拦截解压炸弹。
	maxPosterDimension = 4096
	maxPosterPixels    = 16_777_216
)

var (
	validPosterNameRe  = regexp.MustCompile(`^[0-9a-f]{32}\.(jpg|png|webp|gif)$`)
	posterContentTypes = map[string]string{
		".jpg":  "image/jpeg",
		".png":  "image/png",
		".webp": "image/webp",
		".gif":  "image/gif",
	}
)

// PosterStore 负责本地化持久化保存酒标图片、生成唯一文件名、去重清理并对外提供静态访问。
type PosterStore struct {
	dir string

	mu                sync.RWMutex
	client            *http.Client
	allowPrivateHosts bool
	bgCtx             context.Context
	bgCancel          context.CancelFunc
	bgWG              sync.WaitGroup
	closed            bool
}

func NewPosterStore(dir string) (*PosterStore, error) {
	clean := strings.TrimSpace(dir)
	if clean == "" {
		clean = "posters"
	}
	if err := os.MkdirAll(clean, 0o750); err != nil {
		return nil, fmt.Errorf("创建酒标存储目录 %s 失败: %w", clean, err)
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	s := &PosterStore{
		dir:      clean,
		bgCtx:    bgCtx,
		bgCancel: bgCancel,
	}
	s.client = s.defaultHTTPClient()
	return s, nil
}

// Close 取消仍在执行的异步酒标本地化任务并在超时前等待退出。
func (s *PosterStore) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cancel := s.bgCancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		s.bgWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *PosterStore) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// SetHTTPClientForTest 仅供测试注入 httptest HTTP 客户端，可选允许访问本机回环地址。
func (s *PosterStore) SetHTTPClientForTest(client *http.Client, allowPrivateHosts bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if client != nil {
		s.client = client
	} else {
		s.client = s.defaultHTTPClient()
	}
	s.allowPrivateHosts = allowPrivateHosts
}

func (s *PosterStore) httpClient() (*http.Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil {
		return s.defaultHTTPClient(), s.allowPrivateHosts
	}
	return s.client, s.allowPrivateHosts
}

func (s *PosterStore) defaultHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: platform.NewSafeDialContext(dialer, func() bool {
			_, allowPrivate := s.httpClient()
			return allowPrivate
		}),
	}
	return &http.Client{
		Timeout:   posterFetchTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxPosterRedirects {
				return errors.New("图片跳转次数过多")
			}
			_, allowPrivate := s.httpClient()
			return platform.ValidateOutboundParsedURL(req.URL, "酒标地址", allowPrivate)
		},
	}
}

// ParsePosterFilename 校验并提取本地酒标安全文件名（只允许 32 位 hex + 允许的栅格图片扩展名）。
func ParsePosterFilename(posterURL string) (string, bool) {
	s := strings.TrimSpace(posterURL)
	if !strings.HasPrefix(s, PosterURLPrefix) {
		return "", false
	}
	name := strings.TrimPrefix(s, PosterURLPrefix)
	if !validPosterNameRe.MatchString(name) {
		return "", false
	}
	return name, true
}

// IsLocalPosterURL 判断地址是否指向本系统管理的本地酒标文件。
func IsLocalPosterURL(posterURL string) bool {
	_, ok := ParsePosterFilename(posterURL)
	return ok
}

// DetectPosterExt 按文件头魔数严格识别 JPG / PNG / WebP / GIF，拒绝 SVG、HTML 或伪造扩展名。
func DetectPosterExt(data []byte) (ext string, contentType string, ok bool) {
	switch {
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return ".jpg", posterContentTypes[".jpg"], true
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}):
		return ".png", posterContentTypes[".png"], true
	case len(data) >= 6 && (bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))):
		return ".gif", posterContentTypes[".gif"], true
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return ".webp", posterContentTypes[".webp"], true
	default:
		return "", "", false
	}
}

// SaveBytes 校验图片格式与分辨率后以随机文件名落盘，每次调用都生成互不相同的 URL。
func (s *PosterStore) SaveBytes(data []byte) (string, error) {
	ext, err := s.preparePosterData(data)
	if err != nil {
		return "", err
	}
	name := platform.RandomHex(16) + ext
	if _, err := s.persistPoster(name, data, false); err != nil {
		return "", err
	}
	return PosterURLPrefix + name, nil
}

// saveBytesDedup 以图片内容 SHA-256 前 16 字节命名并复用已有文件，额外返回本次是否新建，供导入失败回滚清理。
func (s *PosterStore) saveBytesDedup(data []byte) (string, bool, error) {
	ext, err := s.preparePosterData(data)
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:16]) + ext
	created, err := s.persistPoster(name, data, true)
	if err != nil {
		return "", false, err
	}
	return PosterURLPrefix + name, created, nil
}

// preparePosterData 校验图片内容：非空、≤5MB、魔数合法，且尺寸不超过防炸弹上限。
func (s *PosterStore) preparePosterData(data []byte) (string, error) {
	if s == nil || s.dir == "" {
		return "", errors.New("酒标存储未初始化")
	}
	if len(data) == 0 {
		return "", platform.UserErr("酒标图片不能为空")
	}
	if len(data) > MaxPosterBytes {
		return "", platform.UserErr("酒标图片不能超过 5MB")
	}
	ext, _, ok := DetectPosterExt(data)
	if !ok {
		return "", platform.UserErr("酒标仅支持 JPG、PNG、WebP 或 GIF 图片（≤ 5MB）")
	}
	if err := validatePosterPixels(data, ext); err != nil {
		return "", err
	}
	return ext, nil
}

// persistPoster 先写临时文件并 fsync，再原子 rename 到目标名，避免读者看到半截图片。
func (s *PosterStore) persistPoster(name string, data []byte, skipExisting bool) (bool, error) {
	dst := filepath.Join(s.dir, name)
	if skipExisting {
		if info, err := os.Stat(dst); err == nil && info.Mode().IsRegular() {
			return false, nil
		}
	}
	tmp, err := os.CreateTemp(s.dir, ".poster-*.tmp")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o640); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return false, err
	}
	cleanupTmp = false
	return true, nil
}

// validatePosterPixels 读取图片头部的尺寸，拒绝超出上限的“解压炸弹”和头信息损坏的伪装图片。
func validatePosterPixels(data []byte, ext string) error {
	width, height := 0, 0
	if ext == ".webp" {
		w, h, ok := webpDimensions(data)
		if !ok {
			return platform.UserErr("酒标图片已损坏或 WebP 头不合法")
		}
		width, height = w, h
	} else {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return platform.UserErr("酒标图片已损坏或格式不支持")
		}
		width, height = cfg.Width, cfg.Height
	}
	if width <= 0 || height <= 0 {
		return platform.UserErr("酒标图片已损坏或尺寸无效")
	}
	if width > maxPosterDimension || height > maxPosterDimension {
		return platform.UserErrf("酒标图片单边不能超过 %d 像素", maxPosterDimension)
	}
	if int64(width)*int64(height) > maxPosterPixels {
		return platform.UserErr("酒标图片总像素过大，请压缩后再上传")
	}
	return nil
}

// webpDimensions 解析 WebP 的 RIFF 容器，支持 VP8X / VP8L / VP8 三种尺寸编码。
func webpDimensions(data []byte) (int, int, bool) {
	if len(data) < 16 || !bytes.Equal(data[:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WEBP")) {
		return 0, 0, false
	}
	total := uint64(len(data))
	for off := 12; off+8 <= len(data); {
		fourcc := string(data[off : off+4])
		chunkSize := uint64(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		avail := total - uint64(off+8)
		bodyLen := chunkSize
		if bodyLen > avail {
			bodyLen = avail
		}
		body := data[off+8 : off+8+int(bodyLen)]
		switch fourcc {
		case "VP8X":
			if len(body) >= 10 {
				w := 1 + int(body[4]) + int(body[5])<<8 + int(body[6])<<16
				h := 1 + int(body[7]) + int(body[8])<<8 + int(body[9])<<16
				return w, h, true
			}
		case "VP8L":
			if len(body) >= 5 && body[0] == 0x2f {
				bits := binary.LittleEndian.Uint32(body[1:5])
				return int(bits&0x3FFF) + 1, int(bits>>14&0x3FFF) + 1, true
			}
		case "VP8 ":
			if len(body) >= 10 && body[3] == 0x9d && body[4] == 0x01 && body[5] == 0x2a {
				w := int(binary.LittleEndian.Uint16(body[6:8]) & 0x3FFF)
				h := int(binary.LittleEndian.Uint16(body[8:10]) & 0x3FFF)
				if w <= 0 || h <= 0 {
					return 0, 0, false
				}
				return w, h, true
			}
		}
		padded := chunkSize + (chunkSize & 1)
		nextU := uint64(off) + 8 + padded
		if nextU > total {
			return 0, 0, false
		}
		off = int(nextU)
	}
	return 0, 0, false
}

func (s *PosterStore) SaveReader(r io.Reader) (string, error) {
	if r == nil {
		return "", platform.UserErr("酒标图片不能为空")
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxPosterBytes+1))
	if err != nil {
		return "", platform.UserErr("读取上传酒标失败，请重试")
	}
	if len(data) > MaxPosterBytes {
		return "", platform.UserErr("酒标图片不能超过 5MB")
	}
	return s.SaveBytes(data)
}

type transientPosterError struct {
	err error
}

func (e transientPosterError) Error() string { return e.err.Error() }
func (e transientPosterError) Unwrap() error { return e.err }

func (s *PosterStore) SaveFromURL(ctx context.Context, rawURL string) (string, error) {
	if s == nil {
		return "", errors.New("酒标存储未初始化")
	}
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", platform.UserErr("酒标图片地址格式不正确")
	}
	client, allowPrivate := s.httpClient()
	if err := platform.ValidateOutboundParsedURL(u, "酒标地址", allowPrivate); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", platform.UserErr("酒标图片地址格式不正确")
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; PourOps/1.0)")
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/*,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		baseErr := platform.UserErr("抓取远程酒标失败，请检查图片链接或改用本地上传")
		if _, isUser := platform.UserErrorMessage(err); !isUser && !errors.Is(err, context.Canceled) {
			return "", transientPosterError{err: baseErr}
		}
		return "", baseErr
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		statusErr := platform.UserErrf("抓取远程酒标失败（状态码 %d），请改用本地上传", resp.StatusCode)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
			return "", transientPosterError{err: statusErr}
		}
		return "", statusErr
	}
	return s.SaveReader(resp.Body)
}

func (s *PosterStore) Exists(posterURL string) bool {
	if s == nil || s.dir == "" {
		return false
	}
	name, ok := ParsePosterFilename(posterURL)
	if !ok {
		return false
	}
	info, err := os.Stat(filepath.Join(s.dir, name))
	return err == nil && info.Mode().IsRegular()
}

func (s *PosterStore) FileInfo(posterURL string) (string, int64, bool) {
	if s == nil || s.dir == "" {
		return "", 0, false
	}
	name, ok := ParsePosterFilename(posterURL)
	if !ok {
		return "", 0, false
	}
	info, err := os.Stat(filepath.Join(s.dir, name))
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxPosterBytes {
		return "", 0, false
	}
	return name, info.Size(), true
}

func (s *PosterStore) Open(posterURL string) (string, *os.File, int64, bool) {
	name, size, ok := s.FileInfo(posterURL)
	if !ok {
		return "", nil, 0, false
	}
	f, err := os.Open(filepath.Join(s.dir, name))
	if err != nil {
		return "", nil, 0, false
	}
	return name, f, size, true
}

func (s *PosterStore) Delete(posterURL string) error {
	if s == nil || s.dir == "" {
		return nil
	}
	name, ok := ParsePosterFilename(posterURL)
	if !ok {
		return nil
	}
	err := os.Remove(filepath.Join(s.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *PosterStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.ServePoster(w, r)
}

func (s *PosterStore) ServePoster(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.dir == "" {
		http.NotFound(w, r)
		return
	}
	name := r.PathValue("name")
	if name == "" && strings.HasPrefix(r.URL.Path, PosterURLPrefix) {
		name = strings.TrimPrefix(r.URL.Path, PosterURLPrefix)
	}
	if !validPosterNameRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.dir, name)
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	if ct := posterContentTypes[strings.ToLower(filepath.Ext(name))]; ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// LookupCatalogPoster 读取历史酒单记录当前保存的 poster_url，便于替换酒标后精准回收旧文件。
func LookupCatalogPoster(ctx context.Context, q platform.Querier, brewery, name string) string {
	if q == nil {
		return ""
	}
	var poster string
	err := q.QueryRow(ctx, `select poster_url from beer_catalog
		where lower(btrim(brewery)) = lower(btrim($1))
		  and lower(btrim(name))    = lower(btrim($2))`,
		strings.TrimSpace(brewery), strings.TrimSpace(name)).Scan(&poster)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(poster)
}

// LockPosterURLTx 在事务内按本地酒标 URL 加事务级咨询锁，避免并发写入与垃圾清理竞争删图。
func LockPosterURLTx(ctx context.Context, tx pgx.Tx, posterURL string) error {
	if tx == nil || !IsLocalPosterURL(posterURL) {
		return nil
	}
	_, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('poster:' || $1))`, strings.TrimSpace(posterURL))
	return err
}

func lockPosterURL(ctx context.Context, q platform.Querier, posterURL string) error {
	tx, ok := q.(pgx.Tx)
	if !ok {
		return nil
	}
	return LockPosterURLTx(ctx, tx, posterURL)
}

// CleanupUnusedPosters 仅当旧酒标既不被在库 beers 引用也不被 beer_catalog 引用时才删除本地文件。
// 每次校验与删文件都在同一咨询锁事务中串行执行，避免与并发保存竞态。
func CleanupUnusedPosters(ctx context.Context, pool *pgxpool.Pool, store *PosterStore, candidates ...string) {
	if pool == nil || store == nil || len(candidates) == 0 {
		return
	}
	// 客户端断开（ctx 取消）不应把孤儿文件留在磁盘上：脱钩取消信号并限时完成清理。
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	seen := make(map[string]struct{}, len(candidates))
	for _, raw := range candidates {
		c := strings.TrimSpace(raw)
		if !IsLocalPosterURL(c) {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}

		tx, err := pool.Begin(cleanCtx)
		if err != nil {
			log.Printf("cleanup unused poster begin (%s): %v", c, err)
			continue
		}
		if err := LockPosterURLTx(cleanCtx, tx, c); err != nil {
			_ = tx.Rollback(cleanCtx)
			log.Printf("cleanup unused poster lock (%s): %v", c, err)
			continue
		}
		var used bool
		err = tx.QueryRow(cleanCtx, `select exists(select 1 from beers where poster_url=$1)
			or exists(select 1 from beer_catalog where poster_url=$1)`, c).Scan(&used)
		if err != nil {
			_ = tx.Rollback(cleanCtx)
			log.Printf("cleanup unused poster (%s): %v", c, err)
			continue
		}
		if !used {
			if err := store.Delete(c); err != nil {
				log.Printf("delete unused poster (%s): %v", c, err)
			}
		}
		_ = tx.Commit(cleanCtx)
	}
}

const localizeWorkers = 3

// LocalizeAllPosters 并发受限（3 worker）地将存量远程 http(s) 酒标拉取并落盘，统一替换 beers 与 beer_catalog 中的路径。
func LocalizeAllPosters(ctx context.Context, pool *pgxpool.Pool, store *PosterStore) int {
	if pool == nil || store == nil {
		return 0
	}
	rows, err := pool.Query(ctx, `select distinct u from (
		select poster_url as u from beers where poster_url ~* '^https?://'
		union
		select poster_url as u from beer_catalog where poster_url ~* '^https?://'
	) t order by u`)
	if err != nil {
		log.Printf("list remote posters: %v", err)
		return 0
	}
	var urls []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			log.Printf("scan remote poster: %v", err)
			return 0
		}
		urls = append(urls, u)
	}
	if err := rows.Err(); err != nil {
		log.Printf("iter remote posters: %v", err)
		return 0
	}

	return localizeURLsBounded(ctx, pool, store, urls)
}

// TriggerAsyncLocalize 在导入等请求提交后后台异步本地化指定远程酒标，不阻塞 HTTP 响应。
func TriggerAsyncLocalize(pool *pgxpool.Pool, store *PosterStore, urls []string) {
	if pool == nil || store == nil || len(urls) == 0 {
		return
	}
	clean := make([]string, 0, len(urls))
	seen := make(map[string]struct{}, len(urls))
	for _, raw := range urls {
		u := strings.TrimSpace(raw)
		if !strings.HasPrefix(strings.ToLower(u), "http") {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		clean = append(clean, u)
	}
	if len(clean) == 0 {
		return
	}
	store.mu.Lock()
	if store.closed {
		store.mu.Unlock()
		return
	}
	parentCtx := store.bgCtx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	store.bgWG.Add(1)
	store.mu.Unlock()

	go func() {
		defer store.bgWG.Done()
		bgCtx, cancel := context.WithTimeout(parentCtx, 2*time.Minute)
		defer cancel()
		localizeURLsBounded(bgCtx, pool, store, clean)
	}()
}

func localizeURLsBounded(ctx context.Context, pool *pgxpool.Pool, store *PosterStore, urls []string) int {
	if store == nil || len(urls) == 0 {
		return 0
	}
	jobs := make(chan string)
	var hostMu sync.Mutex
	hostFails := make(map[string]int)
	var localizedMu sync.Mutex
	localized := 0

	workers := localizeWorkers
	if len(urls) < workers {
		workers = len(urls)
	}
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for remoteURL := range jobs {
				if ctx.Err() != nil {
					return
				}
				host := ""
				if parsed, perr := url.Parse(remoteURL); perr == nil {
					host = strings.ToLower(parsed.Hostname())
				}
				if host != "" {
					hostMu.Lock()
					failedTooMany := hostFails[host] >= 3
					hostMu.Unlock()
					if failedTooMany {
						continue
					}
				}

				localURL, updated, transientFail := localizeOnePosterURL(ctx, pool, store, remoteURL)
				if updated > 0 && localURL != "" {
					if host != "" {
						hostMu.Lock()
						delete(hostFails, host)
						hostMu.Unlock()
					}
					localizedMu.Lock()
					localized++
					localizedMu.Unlock()
				} else if host != "" && transientFail {
					hostMu.Lock()
					hostFails[host]++
					hostMu.Unlock()
				}
			}
		}()
	}

sendLoop:
	for _, remoteURL := range urls {
		select {
		case <-ctx.Done():
			break sendLoop
		case jobs <- remoteURL:
		}
	}
	close(jobs)
	wg.Wait()
	return localized
}

func localizeOnePosterURL(ctx context.Context, pool *pgxpool.Pool, store *PosterStore, remoteURL string) (string, int64, bool) {
	localURL, err := store.SaveFromURL(ctx, remoteURL)
	if err != nil {
		log.Printf("localize poster (%s): %v", remoteURL, err)
		var te transientPosterError
		return "", 0, errors.As(err, &te)
	}
	if pool == nil {
		return localURL, 1, false
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		_ = store.Delete(localURL)
		log.Printf("localize poster tx begin (%s): %v", remoteURL, err)
		return "", 0, false
	}
	defer tx.Rollback(ctx)

	tagBeers, err := tx.Exec(ctx, `update beers set poster_url=$1, updated_at=now() where poster_url=$2`, localURL, remoteURL)
	if err != nil {
		_ = store.Delete(localURL)
		log.Printf("localize beers poster (%s): %v", remoteURL, err)
		return "", 0, false
	}
	tagCatalog, err := tx.Exec(ctx, `update beer_catalog set poster_url=$1, updated_at=now() where poster_url=$2`, localURL, remoteURL)
	if err != nil {
		_ = store.Delete(localURL)
		log.Printf("localize catalog poster (%s): %v", remoteURL, err)
		return "", 0, false
	}
	total := tagBeers.RowsAffected() + tagCatalog.RowsAffected()
	if total == 0 {
		_ = store.Delete(localURL)
		return "", 0, false
	}
	if err := tx.Commit(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		_ = store.Delete(localURL)
		log.Printf("localize poster commit (%s): %v", remoteURL, err)
		return "", 0, false
	}
	return localURL, total, false
}
