package platform

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// FlashCookieName 保存一次性提示信息，避免通过 Redirect Query String 泄漏错误或重放提示。
const FlashCookieName = "bar_flash"

type flashPayload struct {
	Msg string `json:"m,omitempty"`
	Err string `json:"e,omitempty"`
}

func SetFlash(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	if w == nil {
		return
	}
	msg = strings.TrimSpace(msg)
	errMsg = strings.TrimSpace(errMsg)
	if msg == "" && errMsg == "" {
		return
	}
	raw, err := json.Marshal(flashPayload{Msg: msg, Err: errMsg})
	if err != nil {
		return
	}
	val := base64.RawURLEncoding.EncodeToString(raw)
	// 单条 Cookie 上限保留安全余量，避免超出浏览器 4KB Cookie 限制导致静默丢弃。
	if len(val) > 3072 {
		// 按 rune 截断：中文 3 字节/字，按字节判断会把 256 字以内的消息也切爆下标。
		if rs := []rune(msg); len(rs) > 256 {
			msg = string(rs[:256]) + "..."
		}
		if rs := []rune(errMsg); len(rs) > 256 {
			errMsg = string(rs[:256]) + "..."
		}
		raw, err = json.Marshal(flashPayload{Msg: msg, Err: errMsg})
		if err != nil {
			return
		}
		val = base64.RawURLEncoding.EncodeToString(raw)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     FlashCookieName,
		Value:    val,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   IsSecureRequest(r),
		MaxAge:   60,
	})
}

func RedirectMsg(w http.ResponseWriter, r *http.Request, target, msg string) {
	SetFlash(w, r, msg, "")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func RedirectErr(w http.ResponseWriter, r *http.Request, target, errMsg string) {
	SetFlash(w, r, "", errMsg)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func PopFlash(w http.ResponseWriter, r *http.Request) (string, string) {
	if r == nil {
		return "", ""
	}
	c, err := r.Cookie(FlashCookieName)
	if err != nil || c == nil || c.Value == "" {
		return "", ""
	}
	if w != nil {
		http.SetCookie(w, &http.Cookie{
			Name:     FlashCookieName,
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   IsSecureRequest(r),
			MaxAge:   -1,
		})
	}
	return decodeFlashValue(c.Value)
}

func ReadFlashCookies(cookies []*http.Cookie) (string, string) {
	var msg, errMsg string
	for _, c := range cookies {
		if c == nil || c.Name != FlashCookieName || c.MaxAge < 0 || c.Value == "" {
			continue
		}
		m, e := decodeFlashValue(c.Value)
		if m != "" || e != "" {
			msg, errMsg = m, e
		}
	}
	return msg, errMsg
}

func decodeFlashValue(v string) (string, string) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil {
		return "", ""
	}
	var p flashPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", ""
	}
	return p.Msg, p.Err
}
