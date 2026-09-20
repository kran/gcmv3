// 后台 —— 站长工具。
//
// **它不是另一套身份**: 后台 = 前台身份 + 一道角色门（`IsOwner() || HasRole("admin")`）。
// 节点读写走与前台**同一批** `/api/nodes` 端点（能力由身份与读规则决定）; 这里只有
// 后台独有的东西: 界面资源、类型元数据（给动态表单渲染用）、权限矩阵、站点面板。
//
//	GET  /admin/ui/*        界面资源（公开 —— 登录态由 API 层把关）
//	GET  /admin/types       类型定义（前端按 field.kind 取 widgets/<kind>.vue 渲染）
//	GET  /admin/panels      站点/插件注册的面板菜单（每次请求 Fire —— 可响应式查库）
//	GET  /admin/permissions 权限矩阵（拿身份样本真跑一遍策略, 见 permission.go）
//	  + HookAdminMount      插件在这里挂自己的受保护端点
//
// 登录不走后台自己的端点: 前端登录页调 /api/auth/realms 列渠道、再打
// /api/auth/{realm}/login（与前台同一条路, 同一套会话, 同一个 cookie）。
//
// **界面不在服务端描述**: 字段的 kind 名本身就是界面标识（types 声明说了算）。
//
// # 站点面板
//
// 后台自带的只有通用能力（类型的增删改查走 /api/nodes、权限矩阵、账号）。站点特有的
// 管理界面 —— 留言板、报名审核、备份、批量发布 —— 用两个 hook 挂上来, 一共三块:
//
//	① 菜单:　HookAdminPanel（每次 /admin/panels 都 Fire ⇒ 可以按库里的状态动态出现/带角标）
//	② 端点:　HookAdminMount 拿到的是**过了后台门**的路由组, 站点的 API 挂进去就继承了认证
//	③ 组件:　面板的 .vue 也挂在这个组里（**面板代码不该公开**）, URL 由 ① 的 Vue 字段给出
//
//	site.Hook(web.HookAdminPanel, func(ctx *web.CmsCtx, panels *core.List[web.AdminPanel]) error {
//		pending, err := countPending(ctx)   // 真正查一次库
//		if err != nil || pending == 0 { return err }
//		panels.Append(web.AdminPanel{
//			Path: "/review", Title: "内容审核", Vue: "/admin/review/panel.vue",
//		})
//		return nil
//	})
//	site.Hook(web.HookAdminMount, func(g *cho.Cho[*web.CmsCtx]) error {
//		g.Get("/review/pending", func(ctx *web.CmsCtx) { /* ctx.List(...) */ })
//		g.Get("/review/panel.vue", func(ctx *web.CmsCtx) { ctx.String(200, vueSource) })
//		return nil
//	})
//
// 前端不需要任何改动: App.vue 会拉 /admin/panels、动态注册路由与菜单项, 再用
// vue3-sfc-loader 现拉现编译那个 .vue（所以面板也没有构建步骤）。
//
// 两条要点:
//
//	· 面板里的节点读写**走受管入口**（ctx.List / ctx.Get / ctx.Create / ctx.Update /
//	  ctx.Delete）, 策略照旧生效 —— 后台不是特权通道。唯一例外是"系统自己的写"
//	  （导入、迁移、清理）, 那才直接调引擎。
//	· 面板的数据接口用 web.Error / ctx.Fail 报错即可, 形状与 /api/nodes 一致。
//
// 界面资源 embed 进二进制 —— 界面版本与 Go 代码永不脱节（部署只有一个文件）。
package web

import (
	"embed"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"github.com/kran/cho"
	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/types"
)

//go:embed admin
var adminFS embed.FS

// uiFS 界面资源根（embed 的子目录）。
var uiFS, _ = fs.Sub(adminFS, "admin")

const (
	// HookAdminPanel 面板菜单: 站点/插件往后台加自己的页面。
	// 签名: func(*CmsCtx, *core.List[AdminPanel]) error
	HookAdminPanel = "admin.panel"

	// HookAdminMount 挂载后台受保护端点: 插件拿**认证后**的路由组挂东西。
	// 签名: func(*cho.Cho[*CmsCtx]) error
	HookAdminMount = "admin.mount"
)

// AdminPanel 一个后台面板（站点自己的管理页: 菜单项 + vue 组件入口）。
type AdminPanel struct {
	Path  string `json:"path"`  // 组内 API 前缀, 如 "/guestbook"
	Title string `json:"title"` // 后台菜单名
	Vue   string `json:"vue"`   // 面板组件 URL（认证路由, 站点自己挂）
}

func defineAdminHooks(engine core.Engine) {
	err := engine.Hooks().Define(map[string]any{
		HookAdminPanel: func(*CmsCtx, *core.List[AdminPanel]) error { return nil },
		HookAdminMount: func(*cho.Cho[*CmsCtx]) error { return nil },
	})
	if err != nil {
		panic("web: define admin hooks: " + err.Error())
	}
}

// setupAdmin 挂 /admin（Setup 的最后一步 —— 插件挂完自己的端点也就齐了）。
func (s *Site) setupAdmin() {
	s.router.Group("/admin", func(g *cho.Cho[*CmsCtx]) {
		g.Get("/ui/*", adminFile)
		g.Group("", func(authed *cho.Cho[*CmsCtx]) {
			authed.UseCtx(s.requireAdmin)
			authed.Get("/types", s.adminTypes)
			authed.Get("/panels", s.adminPanels)
			authed.Get("/permissions", s.adminPermissions)
			err := s.engine.Hooks().Fire(HookAdminMount, authed)
			if err != nil {
				panic("web: admin mount: " + err.Error())
			}
		})
	})
}

// requireAdmin 后台的门: **有后台权限的人**, 不是"另一种身份"。
//
// owner 与 admin 的分工（D 步定的）: owner 在写侧默认拿到全部字段; admin 只是
// "能进后台"的标识 —— 后台**不给任何读写特权**, 能做什么由站点读规则说了算。
func (s *Site) requireAdmin(ctx *CmsCtx, next func()) {
	actor := ctx.Actor()
	switch {
	case actor.IsAnonymous():
		ctx.Fail(Unauthorized("请先登录"))
	case !actor.IsOwner() && !actor.HasRole(types.RoleAdmin):
		ctx.Fail(Forbidden("需要后台权限"))
	default:
		next()
	}
}

// adminTypes 类型定义（前端按 field.kind 取 widgets/<kind>.vue 渲染表单）。
func (s *Site) adminTypes(ctx *CmsCtx) {
	_ = ctx.Json(http.StatusOK, map[string]any{"types": s.types.Defs()})
}

// adminPanels 站点面板菜单。每次请求 Fire —— 面板可以按库里的状态动态出现。
func (s *Site) adminPanels(ctx *CmsCtx) {
	panels := core.NewList[AdminPanel]()
	err := s.engine.Hooks().Fire(HookAdminPanel, ctx, panels)
	if err != nil {
		ctx.Fail(err)
		return
	}
	_ = ctx.Json(http.StatusOK, map[string]any{"items": panels.Items()})
}

// adminFile 服务界面资源（公开路径 —— 里面没有任何数据）。
//
// 单页应用用的是 hash 路由（`#/nodes` 这种）, 所以不需要服务端兜底路由。
func adminFile(ctx *CmsCtx) {
	ctx.SetHeader("Cache-Control", "no-cache")
	name := path.Clean(strings.TrimPrefix(ctx.R.URL.Path, "/admin/ui/"))
	if name == "." || name == "/" || name == "" {
		name = "index.html"
	}
	data, err := fs.ReadFile(uiFS, name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			ctx.Fail(err)
			return
		}
		ctx.String(http.StatusNotFound, "404 not found")
		return
	}
	contentType := mime.TypeByExtension(filepath.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	ctx.SetHeader("Content-Type", contentType)
	// 界面里没有用户内容, 但仍按"可能被当脚本/样式"处理: 不让浏览器猜类型
	ctx.SetHeader("X-Content-Type-Options", "nosniff")
	_, _ = ctx.W.Write(data)
}
