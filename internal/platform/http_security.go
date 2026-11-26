package platform

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

var (
	outboundCGNATPrefix       = netip.MustParsePrefix("100.64.0.0/10")
	outboundNAT64Prefix       = netip.MustParsePrefix("64:ff9b::/96")
	outboundNAT64LocalPrefix  = netip.MustParsePrefix("64:ff9b:1::/48")
	outbound6to4Prefix        = netip.MustParsePrefix("2002::/16")
	outboundBenchmarkPrefix   = netip.MustParsePrefix("198.18.0.0/15")
	outboundIETFProtoPrefix   = netip.MustParsePrefix("192.0.0.0/24")
	outboundThisNetPrefix     = netip.MustParsePrefix("0.0.0.0/8")
	outboundTestNet1Prefix    = netip.MustParsePrefix("192.0.2.0/24")
	outboundTestNet2Prefix    = netip.MustParsePrefix("198.51.100.0/24")
	outboundTestNet3Prefix    = netip.MustParsePrefix("203.0.113.0/24")
	outboundReservedV4Prefix  = netip.MustParsePrefix("240.0.0.0/4")
	outboundBroadcastAddr     = netip.MustParseAddr("255.255.255.255")
	outboundAliyunMetaAddr    = netip.MustParseAddr("100.100.100.200")
	outboundLinkLocalMetaAddr = netip.MustParseAddr("169.254.169.254")
	outboundAWSMetaV6Addr     = netip.MustParseAddr("fd00:ec2::254")
)

// EnvBool 解析常见布尔环境变量（1 / true / yes）。
func EnvBool(key string) bool {
	v := strings.ToLower(Env(key, ""))
	return v == "1" || v == "true" || v == "yes"
}

// TrustProxy 控制是否信任反向代理头（仅在明确设置 TRUST_PROXY=1/true 时开启）。
func TrustProxy() bool {
	return EnvBool("TRUST_PROXY")
}

// IsSecureRequest 判断请求是否应下发 Secure Cookie（COOKIE_SECURE=1 显式开启、直连 TLS 或受信任反代 HTTPS）。
func IsSecureRequest(r *http.Request) bool {
	if EnvBool("COOKIE_SECURE") {
		return true
	}
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if TrustProxy() {
		parts := strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if proto := strings.TrimSpace(parts[i]); proto != "" {
				return strings.EqualFold(proto, "https")
			}
		}
	}
	return false
}

func isAlwaysBlockedOutboundAddr(addr netip.Addr) bool {
	return addr == outboundAWSMetaV6Addr ||
		addr == outboundLinkLocalMetaAddr ||
		addr == outboundAliyunMetaAddr ||
		addr == outboundBroadcastAddr ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified()
}

// IsBlockedOutboundIP 统一拦截回环、内网、CGNAT、NAT64、6to4、保留网段与云元数据服务；
// 当 allowPrivate=true（测试或内网隧道）时仅放行私网与回环，云元数据与广播/链路本地仍无条件拦截。
func IsBlockedOutboundIP(ip net.IP, allowPrivate bool) bool {
	if ip == nil {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	if isAlwaysBlockedOutboundAddr(addr) {
		return true
	}
	if addr.Is4() {
		if allowPrivate {
			return false
		}
		return addr.IsLoopback() ||
			addr.IsPrivate() ||
			outboundThisNetPrefix.Contains(addr) ||
			outboundCGNATPrefix.Contains(addr) ||
			outboundBenchmarkPrefix.Contains(addr) ||
			outboundIETFProtoPrefix.Contains(addr) ||
			outboundTestNet1Prefix.Contains(addr) ||
			outboundTestNet2Prefix.Contains(addr) ||
			outboundTestNet3Prefix.Contains(addr) ||
			outboundReservedV4Prefix.Contains(addr)
	}
	if addr.IsLoopback() || addr.IsPrivate() {
		return !allowPrivate
	}
	raw := addr.As16()
	if outboundNAT64Prefix.Contains(addr) {
		return IsBlockedOutboundIP(net.IP([]byte{raw[12], raw[13], raw[14], raw[15]}), allowPrivate)
	}
	if outboundNAT64LocalPrefix.Contains(addr) {
		return true
	}
	if outbound6to4Prefix.Contains(addr) {
		return IsBlockedOutboundIP(net.IP([]byte{raw[2], raw[3], raw[4], raw[5]}), allowPrivate)
	}
	// IPv4-compatible ::a.b.c.d（::/96），按内嵌 IPv4 判断（::0 与 ::1 已在前序分支处理）。
	if raw[0]|raw[1]|raw[2]|raw[3]|raw[4]|raw[5]|raw[6]|raw[7]|raw[8]|raw[9]|raw[10]|raw[11] == 0 {
		return IsBlockedOutboundIP(net.IP([]byte{raw[12], raw[13], raw[14], raw[15]}), allowPrivate)
	}
	return false
}

// ValidateOutboundURL 检查出站 HTTP(S) URL：拒绝控制字符、云元数据、IPv6 zone、尾点域名绕过及受限 IP 段。
func ValidateOutboundURL(raw, label string, allowPrivate bool) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return UserErrf("%s格式不正确", label)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return UserErrf("%s包含非法控制字符", label)
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return UserErrf("%s格式不正确", label)
	}
	return ValidateOutboundParsedURL(u, label, allowPrivate)
}

// ValidateOutboundParsedURL 对已解析 URL 做同口径 SSRF 检查，供 HTTP Client Redirect 钩子直接调用。
func ValidateOutboundParsedURL(u *url.URL, label string, allowPrivate bool) error {
	if u == nil || u.Host == "" || u.Hostname() == "" {
		return UserErrf("%s格式不正确", label)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return UserErrf("%s必须以 http:// 或 https:// 开头", label)
	}
	rawHost := strings.TrimSpace(u.Hostname())
	// 先检查原始主机字面量是否带 IPv6 zone，防止 fe80::1%eth0 或 [pub%eth0] 在剥离后逃逸。
	if addr, err := netip.ParseAddr(rawHost); err == nil {
		if addr.Zone() != "" || IsBlockedOutboundIP(addr.Unmap().AsSlice(), allowPrivate) {
			return UserErrf("%s不允许指向本机、内网或保留地址", label)
		}
		return nil
	}
	host := strings.ToLower(strings.TrimRight(rawHost, "."))
	if idx := strings.IndexByte(host, '%'); idx >= 0 {
		return UserErrf("%s不允许包含 IPv6 区域标识", label)
	}
	if host == "metadata.google.internal" || strings.HasSuffix(host, ".metadata.google.internal") ||
		host == "169.254.169.254" || host == "100.100.100.200" || host == "fd00:ec2::254" {
		return UserErrf("%s不能指向云元数据服务", label)
	}
	if !allowPrivate && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
		return UserErrf("%s不允许指向本机或内网地址", label)
	}
	if ip := net.ParseIP(host); ip != nil && IsBlockedOutboundIP(ip, allowPrivate) {
		return UserErrf("%s不允许指向本机、内网或保留地址", label)
	}
	return nil
}

// NewSafeDialContext 在建立出站 TCP 连接前解析 DNS 并校验全部目标 IP，阻断 DNS 重绑定与双宿主绕过。
func NewSafeDialContext(dialer *net.Dialer, allowPrivateFn func() bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		allowPrivate := allowPrivateFn != nil && allowPrivateFn()
		for _, ipAddr := range ips {
			if IsBlockedOutboundIP(ipAddr.IP, allowPrivate) {
				// 脱敏报错：不回显目标内网 IP，避免成为内网探测工具。
				return nil, UserErr("目标地址被安全策略拒绝")
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
		return nil, UserErr("无法解析目标主机地址")
	}
}

// LimitRequestBody 限制写请求体积，防止大请求体耗尽内存；可按精确路由覆写上限。
func LimitRequestBody(defaultMax int64, overrides map[string]int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && defaultMax > 0 {
				limit := defaultMax
				if overrides != nil {
					if v, ok := overrides[r.URL.Path]; ok && v > 0 {
						limit = v
					}
				}
				if r.ContentLength > limit {
					http.Error(w, "请求内容过大", http.StatusRequestEntityTooLarge)
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}
