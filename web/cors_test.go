package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// corsDo 打一个带 Origin 的请求（预检会带上 CORS 预检头）。
func corsDo(t *testing.T, site *Site, method, target, origin string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, site, method, target, func(request *http.Request) {
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if method == http.MethodOptions {
			request.Header.Set("Access-Control-Request-Method", "GET")
			request.Header.Set("Access-Control-Request-Headers", "Authorization")
		}
	})
}

// 没配来源 ⇒ 一点 CORS 头都不带（同源照常工作）。
func TestCORSDisabledByDefault(t *testing.T) {
	site := newPolicySite(t)
	got := corsDo(t, site, http.MethodGet, "/api/auth/realms", "https://spa.example.com")
	if got.Code != http.StatusOK {
		t.Fatalf("请求 = %d", got.Code)
	}
	if got.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("没配来源不该有 CORS 头: %#v", got.Header())
	}
}

// 命中清单: 回显来源（不是 *）+ 带凭据 + Vary。
func TestCORSAllowedOrigin(t *testing.T) {
	site := newPolicySite(t)
	site.CorsOrigins("https://spa.example.com", "https://admin.example.com")
	got := corsDo(t, site, http.MethodGet, "/api/auth/realms", "https://spa.example.com")
	if got.Header().Get("Access-Control-Allow-Origin") != "https://spa.example.com" {
		t.Fatalf("该回显来源: %#v", got.Header())
	}
	if got.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatal("cookie 认证要 Allow-Credentials（回 * 的话浏览器会拒）")
	}
	if !strings.Contains(got.Header().Get("Vary"), "Origin") {
		t.Fatal("该带 Vary: Origin（否则缓存会把 A 站的头发给 B 站）")
	}
	// 末尾斜杠要宽容（站点配置常写成 https://x.com/）
	slash := corsDo(t, site, http.MethodGet, "/api/auth/realms", "https://admin.example.com/")
	if slash.Header().Get("Access-Control-Allow-Origin") != "https://admin.example.com" {
		t.Fatalf("末尾斜杠该被容错: %#v", slash.Header())
	}
}

// 没命中的来源: 普通请求照常但不带 CORS 头; 预检 403 且说明原因。
func TestCORSRejectedOrigin(t *testing.T) {
	site := newPolicySite(t)
	site.CorsOrigins("https://spa.example.com")
	got := corsDo(t, site, http.MethodGet, "/api/auth/realms", "https://evil.example.com")
	if got.Code != http.StatusOK || got.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("没命中的来源不该拿到 CORS 头: %d %#v", got.Code, got.Header())
	}
	preflight := corsDo(t, site, http.MethodOptions, "/api/auth/realms", "https://evil.example.com")
	if preflight.Code != http.StatusForbidden {
		t.Fatalf("预检该 403: %d", preflight.Code)
	}
	if !strings.Contains(preflight.Body.String(), "origin not allowed") {
		t.Fatalf("该说清原因: %q", preflight.Body.String())
	}
}

// 预检: 204 + 方法/头/缓存时间。
func TestCORSPreflight(t *testing.T) {
	site := newPolicySite(t)
	site.CorsOrigins("https://spa.example.com")
	got := corsDo(t, site, http.MethodOptions, "/api/nodes/article", "https://spa.example.com")
	if got.Code != http.StatusNoContent {
		t.Fatalf("预检该 204: %d %q", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Header().Get("Access-Control-Allow-Methods"), "PUT") {
		t.Fatalf("方法该含 PUT: %q", got.Header().Get("Access-Control-Allow-Methods"))
	}
	if got.Header().Get("Access-Control-Allow-Headers") != "Authorization" {
		t.Fatalf("该回显请求的头: %q", got.Header().Get("Access-Control-Allow-Headers"))
	}
	if got.Header().Get("Access-Control-Max-Age") == "" {
		t.Fatal("该带 Max-Age")
	}
}

// 清单写 * ⇒ 允许任何来源, 但**不带**凭据（规范: * 与 Allow-Credentials 互斥）。
func TestCORSAnyOrigin(t *testing.T) {
	site := newPolicySite(t)
	site.CorsOrigins("*")
	got := corsDo(t, site, http.MethodGet, "/api/auth/realms", "https://anything.example.com")
	if got.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("该是 *: %#v", got.Header())
	}
	if got.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("* 不该带 Allow-Credentials")
	}
}

// 只作用于 /api: 静态与探针不带 CORS 头。
func TestCORSOnlyOnAPI(t *testing.T) {
	site := newPolicySite(t)
	site.CorsOrigins("https://spa.example.com")
	for _, target := range []string{"/healthz", "/api/auth/realms"} {
		got := corsDo(t, site, http.MethodGet, target, "https://spa.example.com")
		hasCORS := got.Header().Get("Access-Control-Allow-Origin") != ""
		if target == "/api/auth/realms" && !hasCORS {
			t.Fatalf("%s 该有 CORS 头", target)
		}
		if target == "/healthz" && hasCORS {
			t.Fatalf("%s 不该有 CORS 头（只挂 /api）", target)
		}
	}
}
