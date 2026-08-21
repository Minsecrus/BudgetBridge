package proxy

import (
	"net/url"
	"strings"
	"sync"
)

// effectiveUpstreamCache 记忆化解析结果，键为 (baseUpstream, wsDomain)。两个入参在
// 生产中实质不变——全局 upstream 启动时固定、ws_domain 仅在新增账号时设定（无编辑
// 路径）——因此键空间受账号数（几十个）封顶、条目无需失效。热路径（每次代理尝试）
// 命中时是一次 lock-free 的 sync.Map.Load（外加一次小的 key 字符串拼接，约 1 alloc/op），而不是每请求 url.Parse + u.String()（那会多次分配）。
var effectiveUpstreamCache sync.Map

// effectiveUpstream 返回该账号的上游 base URL（记忆化；外部契约等价于纯函数）。
//
//	wsDomain 空        → 直接返回 baseUpstream（绝大多数账号，行为与今天一致）。
//	wsDomain 含 "://"  → 当完整 base URL 原样用（路径可能不同时用这种）。
//	否则（纯 host）    → 替换 baseUpstream 的 host，保留 scheme + 路径
//	                     （如 /compatible-mode/v1），让业务空间专属域名服务同一 API 面。
//
// baseUpstream 解析失败（err，或 scheme/host 为空）时不猜，回退 baseUpstream。
// key 用 "\x00" 分隔保证不碰撞（URL 不含 NUL 字节）。
func effectiveUpstream(baseUpstream, wsDomain string) string {
	ws := strings.TrimSpace(wsDomain)
	if ws == "" {
		return baseUpstream
	}
	key := baseUpstream + "\x00" + ws
	if v, ok := effectiveUpstreamCache.Load(key); ok {
		return v.(string)
	}
	resolved := resolveUpstream(baseUpstream, ws)
	effectiveUpstreamCache.Store(key, resolved)
	return resolved
}

// resolveUpstream 执行真正的 host 替换 / 原样解析（未缓存），仅由 effectiveUpstream
// 在缓存未命中时调用一次。
func resolveUpstream(baseUpstream, ws string) string {
	if strings.Contains(ws, "://") {
		return strings.TrimRight(ws, "/")
	}
	u, err := url.Parse(baseUpstream)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return baseUpstream
	}
	u.Host = ws
	return u.String()
}
