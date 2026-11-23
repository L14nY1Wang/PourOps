package platform

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"net/url"
	"os"
)

func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// CrossOriginProtect 拦掉浏览器发起的跨站写请求（登录/退出也含在内）；没有 Origin 的本机调用照常放行。
// go.mod 还停在 go 1.24，用不了 http.NewCrossOriginProtection；升到 1.25 后可直接换成它。
func CrossOriginProtect(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			h.ServeHTTP(w, r)
			return
		}
		switch r.Header.Get("Sec-Fetch-Site") {
		case "same-origin", "none":
			// 浏览器已确认同源
		case "":
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					http.Error(w, "跨站请求被拒绝", http.StatusForbidden)
					return
				}
			}
		default:
			http.Error(w, "跨站请求被拒绝", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}
