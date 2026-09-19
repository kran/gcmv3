package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
)

// do 打一个请求穿过完整路由。
func do(t *testing.T, site *Site, method, target string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	for _, opt := range opts {
		opt(request)
	}
	recorder := httptest.NewRecorder()
	site.Setup().ServeHTTP(recorder, request)
	return recorder
}

// 健康探针: /healthz 不碰库（库关掉了也 200）, /readyz 探库且 Close 之后不再就绪。
func TestHealthEndpoints(t *testing.T) {
	site := newTestSite(t)
	if got := do(t, site, http.MethodGet, "/healthz"); got.Code != http.StatusOK || got.Body.String() != "ok" {
		t.Fatalf("healthz = %d %q", got.Code, got.Body.String())
	}
	if got := do(t, site, http.MethodGet, "/readyz"); got.Code != http.StatusOK {
		t.Fatalf("readyz = %d", got.Code)
	}
	// 关掉库: healthz 仍然 200（进程还活着）, readyz 说不可用
	err := site.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := do(t, site, http.MethodGet, "/healthz"); got.Code != http.StatusOK {
		t.Fatalf("healthz 该永 200: %d", got.Code)
	}
	got := do(t, site, http.MethodGet, "/readyz")
	if got.Code != http.StatusServiceUnavailable || !strings.Contains(got.Body.String(), "closing") {
		t.Fatalf("readyz = %d %q", got.Code, got.Body.String())
	}
	// Close 幂等
	if err := site.Close(); err != nil {
		t.Fatal(err)
	}
}

// 静态文件: 能取到, 带 nosniff, 路径穿越拒绝。
func TestServeStatic(t *testing.T) {
	site := newTestSite(t)
	site.Setup() // 建 static/uploads 目录
	dir := filepath.Join(site.basedir, staticDir)
	err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("你好"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "sub", "deep.css"), []byte("body{}"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	got := do(t, site, http.MethodGet, "/static/hello.txt")
	if got.Code != http.StatusOK || got.Body.String() != "你好" {
		t.Fatalf("静态文件 = %d %q", got.Code, got.Body.String())
	}
	if got.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("缺 nosniff: %#v", got.Header())
	}
	// 子目录也服务（/* 匹配多段路径）
	got = do(t, site, http.MethodGet, "/static/sub/deep.css")
	if got.Code != http.StatusOK {
		t.Fatalf("子目录 = %d", got.Code)
	}
	// 站点自己的文件不强制下载（内联是对的）
	if disposition := got.Header().Get("Content-Disposition"); disposition != "" {
		t.Fatalf("/static 不该强制下载: %q", disposition)
	}
	// 路径穿越
	for _, target := range []string{
		"/static/../types.yaml",
		"/static/..%2ftypes.yaml",
		"/static/sub/../../types.yaml",
	} {
		got := do(t, site, http.MethodGet, target)
		if got.Code == http.StatusOK && strings.Contains(got.Body.String(), "types:") {
			t.Fatalf("%s 泄漏了站点文件", target)
		}
	}
}

// 上传目录: 只放图片内联, 其余**强制下载**（同源 XSS 面）。
func TestServeUploadsInlinePolicy(t *testing.T) {
	site := newTestSite(t)
	site.Setup()
	dir := filepath.Join(site.basedir, uploadsDir)
	for _, name := range []string{"logo.png", "doc.svg", "page.html", "evil.svgz"} {
		err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
		if err != nil {
			t.Fatal(err)
		}
	}
	got := do(t, site, http.MethodGet, "/uploads/logo.png")
	if got.Code != http.StatusOK || got.Header().Get("Content-Disposition") != "" {
		t.Fatalf("图片该内联: %d %q", got.Code, got.Header().Get("Content-Disposition"))
	}
	for _, name := range []string{"doc.svg", "page.html", "evil.svgz"} {
		got := do(t, site, http.MethodGet, "/uploads/"+name)
		if got.Code != http.StatusOK {
			t.Fatalf("%s = %d", name, got.Code)
		}
		disposition := got.Header().Get("Content-Disposition")
		if !strings.HasPrefix(disposition, "attachment") {
			t.Fatalf("%s 该强制下载, got %q", name, disposition)
		}
	}
}

// 文件名里的引号/换行不能破坏响应头。
func TestAttachmentHeaderEscaping(t *testing.T) {
	cases := map[string]string{
		`a"b.txt`:    `attachment; filename="ab.txt"`,
		"a\nb.txt":   `attachment; filename="ab.txt"`,
		"中文.txt":     `attachment; filename="__.txt"`,
		`"`:          `attachment; filename="download"`,
		"normal.txt": `attachment; filename="normal.txt"`,
	}
	for name, want := range cases {
		if got := attachmentHeader(name); got != want {
			t.Fatalf("attachmentHeader(%q) = %q, want %q", name, got, want)
		}
	}
}

// HookServeFile 可以改路径 —— 插件拿它做图片处理、私有文件鉴权。
func TestHookServeFileRewritesPath(t *testing.T) {
	site := newPolicySite(t)
	site.Hook(HookServeFile, func(ctx *CmsCtx, path *string) error {
		if ctx.Actor().IsAnonymous() {
			return Forbidden("需要登录")
		}
		if filepath.Base(*path) == "alias.txt" {
			*path = filepath.Join(filepath.Dir(*path), "real.txt")
		}
		return nil
	})
	site.Setup()
	dir := filepath.Join(site.basedir, staticDir)
	err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("真的"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// 匿名: 插件拒绝（403 来自插件自己的错误, 不是框架）
	got := do(t, site, http.MethodGet, "/static/alias.txt")
	if got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "需要登录") {
		t.Fatalf("匿名 = %d %q", got.Code, got.Body.String())
	}
	// 已认证: 路径被改写到 real.txt
	id, err := site.Engine().RegisterAuth(nil, "member", "email", "a@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "frontend", id)
	if err != nil {
		t.Fatal(err)
	}
	got = do(t, site, http.MethodGet, "/static/alias.txt", withBearer(token))
	if got.Code != http.StatusOK || got.Body.String() != "真的" {
		t.Fatalf("已认证 = %d %q", got.Code, got.Body.String())
	}
}

// HookBeforeMount 先于内置路由执行 —— 插件因此能挂中间件与自己的路由。
func TestHookBeforeMountOrder(t *testing.T) {
	site := newTestSite(t)
	var order []string
	site.Hook(HookBeforeMount, func(s *Site) error {
		order = append(order, "hook")
		// 中间件（在路由注册之前 —— chi 之后 Use 会 panic）
		s.Router().UseStd(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Plugin", "1")
				next.ServeHTTP(w, r)
			})
		})
		// 插件自己的路由
		s.Router().Get("/plugin/hello", func(ctx *CmsCtx) {
			ctx.String(http.StatusOK, "插件路由")
		})
		return nil
	})
	got := do(t, site, http.MethodGet, "/plugin/hello")
	if got.Code != http.StatusOK || got.Body.String() != "插件路由" {
		t.Fatalf("插件路由 = %d %q", got.Code, got.Body.String())
	}
	if got.Header().Get("X-Plugin") != "1" {
		t.Fatal("插件中间件该对整个站点生效")
	}
	// 内置路由也在（挂载顺序: hook 在前）
	if got := do(t, site, http.MethodGet, "/healthz"); got.Code != http.StatusOK {
		t.Fatalf("内置路由 = %d", got.Code)
	}
	if len(order) != 1 {
		t.Fatalf("hook 该只跑一次: %#v", order)
	}
}

// Setup 幂等: 重复调用不会挂两遍路由。
func TestSetupIdempotent(t *testing.T) {
	site := newTestSite(t)
	calls := 0
	site.Hook(HookBeforeMount, func(*Site) error {
		calls++
		return nil
	})
	first := site.Setup()
	second := site.Setup()
	if first != second {
		t.Fatal("重复 Setup 该返回同一个 handler")
	}
	if calls != 1 {
		t.Fatalf("hook 该只触发一次: %d", calls)
	}
	if got := do(t, site, http.MethodGet, "/healthz"); got.Code != http.StatusOK {
		t.Fatalf("healthz = %d", got.Code)
	}
}

// Setup 之后策略冻结。
func TestPoliciesFrozenAfterStart(t *testing.T) {
	site := newTestSite(t)
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	site.Setup()
	defer func() {
		if recover() == nil {
			t.Fatal("Setup 之后注册策略该 panic")
		}
	}()
	site.Type("article")
}

// 未知路由: 走到路由器的 404（不是 panic、不是 200）。
func TestUnknownRoute(t *testing.T) {
	site := newTestSite(t)
	got := do(t, site, http.MethodGet, "/nope")
	if got.Code != http.StatusNotFound {
		t.Fatalf("未知路由 = %d", got.Code)
	}
}

// Hook 注册签名不匹配 / 事件名不存在 ⇒ 当场 panic（不静默失效）。
func TestHookRegistrationPanics(t *testing.T) {
	cases := []struct {
		name string
		fn   any
	}{
		{"签名不匹配", func(int) error { return nil }},
		{"事件名不存在", nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			site := newTestSite(t)
			defer func() {
				if recover() == nil {
					t.Fatal("该 panic")
				}
			}()
			name := HookBeforeMount
			if test.fn == nil {
				name = "web.nope"
			}
			site.Hook(name, test.fn)
		})
	}
}

// 注册一个合法的 hook 后 Fire 真的能拿到站点。
func TestHookFires(t *testing.T) {
	site := newTestSite(t)
	var seen *Site
	site.Hook(HookBeforeMount, func(s *Site) error {
		seen = s
		return nil
	})
	site.Setup()
	if seen != site {
		t.Fatalf("hook 该拿到站点: %#v", seen)
	}
	// hook 返回错误 ⇒ 启动期响亮地炸（不是静默忽略）
	broken := newTestSite(t)
	broken.Hook(HookBeforeMount, func(*Site) error { return errors.New("插件没配好") })
	defer func() {
		got := recover()
		if got == nil || !strings.Contains(got.(string), "插件没配好") {
			t.Fatalf("该带着原因 panic: %v", got)
		}
	}()
	broken.Setup()
}

// 引擎真的能在站点里用（端到端: 建节点 → 走 API 之前的最后一次真机核对）。
func TestSiteEndToEnd(t *testing.T) {
	site := newTestSite(t)
	site.Setup()
	_, err := site.Engine().CreateNode(nil, &core.Node{Type: "article", Fields: core.Fields{"title": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	// 站点的直方图: 建了目录、起了路由、探针可用
	for _, dir := range []string{staticDir, uploadsDir} {
		info, err := os.Stat(filepath.Join(site.basedir, dir))
		if err != nil || !info.IsDir() {
			t.Fatalf("%s 该被自动建出来: %v", dir, err)
		}
	}
	if got := do(t, site, http.MethodGet, "/readyz"); got.Code != http.StatusOK {
		t.Fatalf("readyz = %d", got.Code)
	}
}
