// CORS —— API 的跨源访问（**内置**, 不需要插件）。
//
// 站点在配置期给一份允许来源清单:
//
//	site.CorsOrigins("https://assoc.example.com")
//
// 语义:
//
//	没配        什么都不做（同源照常, 不带任何 CORS 头）
//	命中清单    回显该 Origin（**不是 `*`** —— 带 cookie 认证时浏览器拒收 `*`）
//	            + `Access-Control-Allow-Credentials: true`
//	清单里有 `*` 允许任何来源, 但**不带** Allow-Credentials（规范如此: `*` 与凭据互斥）
//	没命中      不写 CORS 头（浏览器自己拦）; 预检请求回 403 并说明原因
//
// 预检（OPTIONS）在这里直接答复 —— 路由表里没有这些 OPTIONS 端点。
//
// 中间件**全局**挂（预检 OPTIONS 没有匹配的路由, 作用域中间件根本不触发）, 但它自己
// 把范围限到 `/api`: 后台界面与静态资源本来就是同源用的, 不需要跨源头。
package web

import (
	"net/http"
	"strings"
)

// CorsOrigins 允许跨源访问 API 的来源清单（配置期设置; 传空 = 不启用）。
//
// 写 "*" 表示允许任何来源（此时不带凭据 —— 见文件头）。
func (s *Site) CorsOrigins(origins ...string) {
	s.corsOrigins = map[string]bool{}
	s.corsAny = false
	for _, origin := range origins {
		origin = strings.TrimRight(strings.TrimSpace(origin), "/")
		if origin == "" {
			continue
		}
		if origin == "*" {
			s.corsAny = true
			continue
		}
		s.corsOrigins[origin] = true
	}
}

// corsEnabled 配过来源才挂中间件（没配就一点开销都没有）。
func (s *Site) corsEnabled() bool { return s.corsAny || len(s.corsOrigins) > 0 }

// corsMiddleware 全局中间件（在 Setup 里、任何路由注册之前追加）; 只处理 /api。
func (s *Site) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api" {
			next.ServeHTTP(w, r)
			return
		}
		origin := strings.TrimRight(r.Header.Get("Origin"), "/")
		if origin == "" {
			next.ServeHTTP(w, r) // 同源请求 / 非浏览器客户端
			return
		}
		if !s.corsAny && !s.corsOrigins[origin] {
			if r.Method == http.MethodOptions {
				// 预检被拒: 明确回一个原因, 别让前端只看到"请求失败"
				http.Error(w, "cors: origin not allowed: "+origin, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		header := w.Header()
		header.Add("Vary", "Origin")
		if s.corsAny {
			header.Set("Access-Control-Allow-Origin", "*")
		} else {
			header.Set("Access-Control-Allow-Origin", origin)
			header.Set("Access-Control-Allow-Credentials", "true")
		}
		// 让前端读得到限速的等待时间（默认只有少数字段可读）
		header.Set("Access-Control-Expose-Headers", "Retry-After")
		if r.Method != http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		header.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		requested := r.Header.Get("Access-Control-Request-Headers")
		if requested == "" {
			requested = "Authorization, Content-Type"
		}
		header.Set("Access-Control-Allow-Headers", requested)
		header.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
	})
}
