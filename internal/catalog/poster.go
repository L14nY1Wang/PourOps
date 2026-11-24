package catalog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
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
)

var (
	validPosterNameRe  = regexp.MustCompile(`^[0-9a-f]{32}\.(jpg|png|webp|gif)$`)
	cgnatPrefix        = netip.MustParsePrefix("100.64.0.0/10")
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
}

func NewPosterStore(dir string) (*PosterStore, error) {
	clean := strings.TrimSpace(dir)
	if clean == "" {
		clean = "posters"
	}
	if err := os.MkdirAll(clean, 0o750); err != nil {
		return nil, fmt.Errorf("创建酒标存储目录 %s 失败: %w", clean, err)
	}
	s := &PosterStore{dir: clean}
	s.client = s.defaultHTTPClient()
	return s, nil
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
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ipAddr := range ips {
				if isBlockedPosterIP(ipAddr.IP) {
					return nil, fmt.Errorf("拒绝访问内网或受限地址: %s", ipAddr.IP.String())
				}
			}
			var lastErr error
			for _, ipAddr := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, errors.New("无法解析目标主机地址")
		},
	}
	return &http.Client{
		Timeout:   posterFetchTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxPosterRedirects {
				return errors.New("图片跳转次数过多")
			}
			_, allowPrivate := s.httpClient()
			return validateRemotePosterURL(req.URL, allowPrivate)
		},
	}
}

func isBlockedPosterIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	return addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified() ||
		cgnatPrefix.Contains(addr)
}

func validateRemotePosterURL(u *url.URL, allowPrivateHosts bool) error {
	if u == nil || u.Host == "" || u.Hostname() == "" {
		return platform.UserErr("酒标图片地址格式不正确")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return platform.UserErr("远程酒标地址必须以 http:// 或 https:// 开头")
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "metadata.google.internal" || strings.HasSuffix(host, ".metadata.google.internal") || host == "169.254.169.254" {
		return platform.UserErr("酒标地址不能指向云元数据服务")
	}
	if !allowPrivateHosts {
		if host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return platform.UserErr("酒标地址不允许指向本机或内网地址")
		}
		if ip := net.ParseIP(host); ip != nil && isBlockedPosterIP(ip) {
			return platform.UserErr("酒标地址不允许指向本机或内网地址")
		}
	}
	return nil
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

func (s *PosterStore) SaveBytes(data []byte) (string, error) {
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

	name := platform.RandomHex(16) + ext
	tmp, err := os.CreateTemp(s.dir, ".poster-*.tmp")
	if err != nil {
		return "", err
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
		return "", err
	}
	if err := tmp.Chmod(0o640); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	dst := filepath.Join(s.dir, name)
	if err := os.Rename(tmpName, dst); err != nil {
		return "", err
	}
	cleanupTmp = false
	return PosterURLPrefix + name, nil
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
	if err := validateRemotePosterURL(u, allowPrivate); err != nil {
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
		return "", platform.UserErr("抓取远程酒标失败，请检查图片链接或改用本地上传")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", platform.UserErrf("抓取远程酒标失败（状态码 %d），请改用本地上传", resp.StatusCode)
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

func (s *PosterStore) Read(posterURL string) (string, []byte, bool) {
	if s == nil || s.dir == "" {
		return "", nil, false
	}
	name, ok := ParsePosterFilename(posterURL)
	if !ok {
		return "", nil, false
	}
	path := filepath.Join(s.dir, name)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxPosterBytes {
		return "", nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > MaxPosterBytes {
		return "", nil, false
	}
	return name, data, true
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

// CleanupUnusedPosters 仅当旧酒标既不被在库 beers 引用也不被 beer_catalog 引用时才删除本地文件。
func CleanupUnusedPosters(ctx context.Context, pool *pgxpool.Pool, store *PosterStore, candidates ...string) {
	if pool == nil || store == nil || len(candidates) == 0 {
		return
	}
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

		var used bool
		err := pool.QueryRow(ctx, `select exists(select 1 from beers where poster_url=$1)
			or exists(select 1 from beer_catalog where poster_url=$1)`, c).Scan(&used)
		if err != nil {
			log.Printf("cleanup unused poster (%s): %v", c, err)
			continue
		}
		if !used {
			if err := store.Delete(c); err != nil {
				log.Printf("delete unused poster (%s): %v", c, err)
			}
		}
	}
}

// LocalizeAllPosters 在服务启动后异步把存量远程 http(s) 酒标拉取并落盘，统一替换 beers 与 beer_catalog 中的路径。
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

	localized := 0
	for _, remoteURL := range urls {
		if ctx.Err() != nil {
			break
		}
		localURL, updated := localizeOnePosterURL(ctx, pool, store, remoteURL)
		if updated > 0 && localURL != "" {
			localized++
		}
	}
	return localized
}

// LocalizeCatalogPosters 在导出 ZIP 压缩包前尽力把尚未本地化的远程酒标拉取到本地并写回数据库。
func LocalizeCatalogPosters(ctx context.Context, pool *pgxpool.Pool, store *PosterStore, items []CatalogBeer) {
	if store == nil || len(items) == 0 {
		return
	}
	resolved := make(map[string]string)
	for i := range items {
		raw := strings.TrimSpace(items[i].PosterURL)
		if !strings.HasPrefix(strings.ToLower(raw), "http") {
			continue
		}
		if cached, seen := resolved[raw]; seen {
			if cached != "" {
				items[i].PosterURL = cached
			}
			continue
		}
		localURL, _ := localizeOnePosterURL(ctx, pool, store, raw)
		resolved[raw] = localURL
		if localURL != "" {
			items[i].PosterURL = localURL
		}
	}
}

func localizeOnePosterURL(ctx context.Context, pool *pgxpool.Pool, store *PosterStore, remoteURL string) (string, int64) {
	localURL, err := store.SaveFromURL(ctx, remoteURL)
	if err != nil {
		log.Printf("localize poster (%s): %v", remoteURL, err)
		return "", 0
	}
	if pool == nil {
		return localURL, 1
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		_ = store.Delete(localURL)
		log.Printf("localize poster tx begin (%s): %v", remoteURL, err)
		return "", 0
	}
	defer tx.Rollback(ctx)

	tagBeers, err := tx.Exec(ctx, `update beers set poster_url=$1, updated_at=now() where poster_url=$2`, localURL, remoteURL)
	if err != nil {
		_ = store.Delete(localURL)
		log.Printf("localize beers poster (%s): %v", remoteURL, err)
		return "", 0
	}
	tagCatalog, err := tx.Exec(ctx, `update beer_catalog set poster_url=$1, updated_at=now() where poster_url=$2`, localURL, remoteURL)
	if err != nil {
		_ = store.Delete(localURL)
		log.Printf("localize catalog poster (%s): %v", remoteURL, err)
		return "", 0
	}
	total := tagBeers.RowsAffected() + tagCatalog.RowsAffected()
	if total == 0 {
		_ = store.Delete(localURL)
		return "", 0
	}
	if err := tx.Commit(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		_ = store.Delete(localURL)
		log.Printf("localize poster commit (%s): %v", remoteURL, err)
		return "", 0
	}
	return localURL, total
}
