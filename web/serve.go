// 启动与内置路由。
//
// Setup 的顺序就是**契约**:
//
//	① HookBeforeMount —— 插件/站点挂中间件与自己的路由（chi 的路由之后再 Use 会 panic,
//	   所以这是中间件的唯一正确时机）
//	② 静态文件（/static 站点自己的, /uploads 用户上传的）
//	③ 健康探针（/healthz /readyz）
//	④ 通用 API（/api, E 步）
//
// 幂等: 重复 Setup 不会挂两遍路由（sync.Once）。
package web

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// 站点固定目录（basedir 下, 不存在会自动建）。
const (
	staticDir  = "static"
	uploadsDir = "uploads"
)

// Setup 挂载全部内置路由, 返回 http.Handler（直接 serve）。幂等, 可重复调用。
func (s *Site) Setup() http.Handler {
	s.setupOnce.Do(func() {
		s.started = true // 策略此后冻结
		err := s.engine.Hooks().Fire(HookBeforeMount, s)
		if err != nil {
			panic("web: before mount: " + err.Error())
		}
		s.setupFiles(filepath.Join(s.basedir, staticDir), "/static/*", "/static/", false)
		s.setupFiles(filepath.Join(s.basedir, uploadsDir), "/uploads/*", "/uploads/", true)
		s.setupHealth()
	})
	return s.router
}

// Start 单站便捷入口 —— 等价 Setup（返回 http.Handler, 直接交给 http.Server）。
func (s *Site) Start() http.Handler { return s.Setup() }

// setupHealth 运维探针。
//
//	/healthz 进程活着 —— **不碰数据库**, 永远 200（库挂了也要能回答"进程还在"）。
//	/readyz  可以接客 —— 探库 + Close 之后不再就绪。
//
// 两者都不暴露版本/配置/SQLite 细节, 无认证也可以公开。
func (s *Site) setupHealth() {
	s.router.Get("/healthz", func(ctx *CmsCtx) {
		ctx.String(http.StatusOK, "ok")
	})
	s.router.Get("/readyz", func(ctx *CmsCtx) {
		if !s.alive.Load() {
			ctx.String(http.StatusServiceUnavailable, "closing")
			return
		}
		err := s.db.Pool().PingContext(ctx.R.Context())
		if err != nil {
			slog.Error("web: readyz: database ping", "err", err)
			ctx.String(http.StatusServiceUnavailable, "database unavailable")
			return
		}
		ctx.String(http.StatusOK, "ready")
	})
}

// setupFiles 建目录（不存在自动建）+ 挂文件服务端点。
func (s *Site) setupFiles(dir, pattern, prefix string, userContent bool) {
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		panic("web: mkdir " + dir + ": " + err.Error())
	}
	s.serveFiles(dir, pattern, prefix, userContent)
}

// serveFiles 服务一个目录。路径可被 HookServeFile 改写（图片处理、私有文件鉴权）。
func (s *Site) serveFiles(dir, pattern, prefix string, userContent bool) {
	s.router.Get(pattern, func(ctx *CmsCtx) {
		rel := strings.TrimPrefix(ctx.R.URL.Path, prefix)
		if strings.Contains(rel, "..") {
			ctx.String(http.StatusNotFound, "404 not found")
			return
		}
		path := filepath.Join(dir, filepath.FromSlash(rel))
		err := s.engine.Hooks().Fire(HookServeFile, ctx, &path)
		if err != nil {
			ctx.Fail(err) // 插件返回 *Error 就是它的状态码, 别的算 500
			return
		}
		// 不让浏览器猜 MIME（猜出来的类型会绕过扩展名策略）
		ctx.SetHeader("X-Content-Type-Options", "nosniff")
		if userContent && !inlineSafe(path) {
			// 用户上传的内容与站点**同源**: .svg / .html / .js 内联渲染等于允许
			// 在站点的域里跑别人写的脚本。白名单之外一律下载, 不渲染。
			ctx.SetHeader("Content-Disposition", attachmentHeader(filepath.Base(path)))
		}
		http.ServeFile(ctx.W, ctx.R, path)
	})
}

// inlineSafe 允许内联渲染的上传扩展名 —— **只放图片**。
//
// 白名单而不是黑名单: 要维护"哪些扩展名危险"的清单早晚会漏（.svgz? .xhtml?
// 某个浏览器新支持的容器格式?）。漏一个就是同源 XSS。
func inlineSafe(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".bmp", ".ico":
		return true
	}
	return false
}

// attachmentHeader 造 Content-Disposition。文件名里可能有引号/换行 ⇒ 去掉会破坏
// 头的字符, 再退化成纯 ASCII 兜底名。
func attachmentHeader(name string) string {
	safe := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return -1
		}
		if r > 0x7f {
			return '_'
		}
		return r
	}, name)
	if safe == "" {
		safe = "download"
	}
	return `attachment; filename="` + safe + `"`
}
