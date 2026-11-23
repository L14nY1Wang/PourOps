package ui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

const AdminBundlePath = "/static/css/admin.bundle.css"

// AssetManifest 对内嵌静态资源计算内容指纹，并把 static/css/*.css 内存拼成单束 CSS。
// 其余静态文件由内置 FileServer 原样输出。
type AssetManifest struct {
	staticRoot  fs.FS
	adminBundle []byte
	bundleHash  string
	urls        map[string]string // "/static/js/foo.js" -> "/static/js/foo.js?v=<hash>"
	fileHashes  map[string]string // "/static/js/foo.js" -> "<hash>"
	fileServer  http.Handler
}

// NewAssetManifest 基于以 static 为根的 fs.FS 构建清单。
func NewAssetManifest(staticRoot fs.FS) (*AssetManifest, error) {
	m := &AssetManifest{
		staticRoot: staticRoot,
		urls:       make(map[string]string),
		fileHashes: make(map[string]string),
		fileServer: http.StripPrefix("/static/", http.FileServer(http.FS(staticRoot))),
	}

	// 1. 拼接 css/*.css
	adminEntries, err := fs.ReadDir(staticRoot, "css")
	if err == nil {
		var cssFiles []string
		for _, e := range adminEntries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".css") {
				cssFiles = append(cssFiles, e.Name())
			}
		}
		sort.Strings(cssFiles)
		var buf bytes.Buffer
		for i, name := range cssFiles {
			data, err := fs.ReadFile(staticRoot, path.Join("css", name))
			if err != nil {
				return nil, err
			}
			if i > 0 && buf.Len() > 0 && !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
				buf.WriteByte('\n')
			}
			buf.Write(data)
		}
		m.adminBundle = buf.Bytes()
		m.bundleHash = shortDigest(m.adminBundle)
		bundleURL := AdminBundlePath + "?v=" + m.bundleHash
		m.urls[AdminBundlePath] = bundleURL
		m.urls["/static/css/admin.css"] = bundleURL
		m.fileHashes[AdminBundlePath] = m.bundleHash
		m.fileHashes["/static/css/admin.css"] = m.bundleHash
	}

	// 2. 为其余所有内嵌静态文件登记内容短哈希
	if err := fs.WalkDir(staticRoot, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		raw, err := fs.ReadFile(staticRoot, p)
		if err != nil {
			return err
		}
		h := shortDigest(raw)
		key := "/static/" + p
		m.fileHashes[key] = h
		m.urls[key] = key + "?v=" + h
		return nil
	}); err != nil {
		return nil, err
	}

	return m, nil
}

func shortDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:8]
}

// URL 返回带自动内容哈希查询参数的静态资源地址；用于模板函数 {{asset "/static/..."}}。
func (m *AssetManifest) URL(assetPath string) string {
	if m == nil {
		return assetPath
	}
	if u, ok := m.urls[assetPath]; ok {
		return u
	}
	return assetPath
}

// Handler 提供 /static/ 前缀服务：对打包样式输出 adminBundle，对其余静态文件走内嵌 FS 并命中哈希强缓存。
func (m *AssetManifest) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(m.adminBundle) > 0 && (p == AdminBundlePath || p == "/static/css/admin.css") {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			w.Header().Set("ETag", `"`+m.bundleHash+`"`)
			if r.URL.Query().Get("v") == m.bundleHash {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			http.ServeContent(w, r, "admin.bundle.css", time.Time{}, bytes.NewReader(m.adminBundle))
			return
		}
		if h, ok := m.fileHashes[p]; ok && r.URL.Query().Get("v") == h {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		m.fileServer.ServeHTTP(w, r)
	})
}
