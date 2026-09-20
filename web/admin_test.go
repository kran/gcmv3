package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kran/cho"
	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// adminSite 一个能开后台的站点: 会员（前台）+ 员工（后台管理员）。
func adminSite(t *testing.T) *Site {
	t.Helper()
	site := newPolicySite(t)
	site.Auth().Register(AuthRealm{Name: "staff", NodeType: "staff"})
	openAll := func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	}
	site.Type("article").OnRead(openAll)
	site.Type("member").OnRead(openAll)
	site.Type("staff").OnRead(openAll)
	return site
}

// adminToken 造一个员工/管理员身份, 返回 (令牌, 节点 id)。
func adminToken(t *testing.T, site *Site, roles ...string) (string, int64) {
	t.Helper()
	nodeRoles := make([]any, 0, len(roles))
	for _, role := range roles {
		nodeRoles = append(nodeRoles, role)
	}
	// 同一个夹具可能造多个身份 ⇒ 标识得各不相同（凭据是唯一的）
	identifier := "boss"
	if len(roles) > 0 {
		identifier += "-" + strings.Join(roles, "-")
	}
	id, err := site.Engine().RegisterAuth(nil, "staff", "username", identifier,
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "老板", "roles": nodeRoles}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "staff", id)
	if err != nil {
		t.Fatal(err)
	}
	return token, id
}

// 门: 匿名 401、已登录但没角色 403、owner/admin 放行。
func TestAdminGate(t *testing.T) {
	site := adminSite(t)
	// 匿名
	got := do(t, site, http.MethodGet, "/admin/types")
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 = %d %q", got.Code, got.Body.String())
	}
	// 普通会员（前台身份, 没有后台角色）
	token, _ := adminToken(t, site) // 没有 owner/admin 角色
	got = do(t, site, http.MethodGet, "/admin/types", withBearer(token))
	if got.Code != http.StatusForbidden {
		t.Fatalf("普通员工 = %d %q", got.Code, got.Body.String())
	}
	// admin 角色放行
	adminTokenValue, _ := adminToken(t, site, types.RoleAdmin)
	got = do(t, site, http.MethodGet, "/admin/types", withBearer(adminTokenValue))
	if got.Code != http.StatusOK {
		t.Fatalf("admin = %d %q", got.Code, got.Body.String())
	}
	// owner 角色放行
	owner, _ := adminToken(t, site, types.RoleOwner)
	got = do(t, site, http.MethodGet, "/admin/panels", withBearer(owner))
	if got.Code != http.StatusOK {
		t.Fatalf("owner = %d %q", got.Code, got.Body.String())
	}
}

// 界面资源: embed 进二进制, 无需站点目录; 根路径给 index.html; 缺文件 404。
func TestAdminUI(t *testing.T) {
	site := adminSite(t)
	// index.html（不需要登录 —— 它是空壳, 数据全在 API 层把关）
	got := do(t, site, http.MethodGet, "/admin/ui/")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "panel-app") {
		t.Fatalf("index = %d %q", got.Code, got.Body.String()[:min(80, got.Body.Len())])
	}
	if !strings.Contains(got.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("content-type = %q", got.Header().Get("Content-Type"))
	}
	if got.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("cache-control = %q", got.Header().Get("Cache-Control"))
	}
	// 子资源（js/vendor 都在同一个 embed 树里）
	got = do(t, site, http.MethodGet, "/admin/ui/js/api.js")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "Panel") {
		t.Fatalf("js = %d", got.Code)
	}
	// 缺文件
	got = do(t, site, http.MethodGet, "/admin/ui/nope.js")
	if got.Code != http.StatusNotFound {
		t.Fatalf("缺文件 = %d", got.Code)
	}
	// _tools 不该被服务（embed 不含 _ 前缀, 也不该含）
	got = do(t, site, http.MethodGet, "/admin/ui/_tools/check.js")
	if got.Code != http.StatusNotFound {
		t.Fatalf("_tools 不该对外: %d", got.Code)
	}
}

// /admin/types 给前端渲染表单用: 每个字段的 kind 就是界面标识。
func TestAdminTypes(t *testing.T) {
	site := adminSite(t)
	token, _ := adminToken(t, site, types.RoleOwner)
	got := do(t, site, http.MethodGet, "/admin/types", withBearer(token))
	if got.Code != http.StatusOK {
		t.Fatalf("types = %d", got.Code)
	}
	var payload struct {
		Types map[string]types.TypeDef `json:"types"`
	}
	err := json.Unmarshal(got.Body.Bytes(), &payload)
	if err != nil {
		t.Fatal(err)
	}
	article, ok := payload.Types["article"]
	if !ok {
		t.Fatalf("没有 article: %#v", payload.Types)
	}
	if len(article.Fields) == 0 || article.Fields[0].Kind == "" {
		t.Fatalf("字段该带 kind: %#v", article.Fields)
	}
	// 角色字段是能力注入的（前台渲染要有它才能编角色）
	if _, ok := site.Types().Field("member", types.RolesField); !ok {
		t.Fatal("可登录类型该有注入的 roles 字段")
	}
}

// 面板菜单: 站点/插件热插拔（每次请求 Fire）。
func TestAdminPanels(t *testing.T) {
	site := adminSite(t)
	site.Hook(HookAdminPanel, func(_ *CmsCtx, panels *core.List[AdminPanel]) error {
		panels.Append(AdminPanel{Path: "/guestbook", Title: "留言板", Vue: "/admin/ui/panels/guestbook.vue"})
		return nil
	})
	token, _ := adminToken(t, site, types.RoleOwner)
	got := do(t, site, http.MethodGet, "/admin/panels", withBearer(token))
	if got.Code != http.StatusOK {
		t.Fatalf("panels = %d %q", got.Code, got.Body.String())
	}
	for _, want := range []string{"留言板", "/guestbook"} {
		if !strings.Contains(got.Body.String(), want) {
			t.Fatalf("面板列表缺 %s: %q", want, got.Body.String())
		}
	}
	// 没有任何面板时是空数组（不是 null）
	empty := adminSite(t)
	token2, _ := adminToken(t, empty, types.RoleOwner)
	got = do(t, empty, http.MethodGet, "/admin/panels", withBearer(token2))
	if !strings.Contains(got.Body.String(), `"items":[]`) {
		t.Fatalf("空面板 = %q", got.Body.String())
	}
}

// HookAdminMount: 插件挂**受保护**端点（门已经装好了）。
func TestAdminMount(t *testing.T) {
	site := adminSite(t)
	site.Hook(HookAdminMount, func(g *cho.Cho[*CmsCtx]) error {
		g.Get("/dashboard", func(ctx *CmsCtx) {
			ctx.String(http.StatusOK, "面板数据")
		})
		return nil
	})
	// 没登录 ⇒ 被门拦下（插件端点继承认证）
	got := do(t, site, http.MethodGet, "/admin/dashboard")
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("插件端点该继承门: %d", got.Code)
	}
	token, _ := adminToken(t, site, types.RoleAdmin)
	got = do(t, site, http.MethodGet, "/admin/dashboard", withBearer(token))
	if got.Code != http.StatusOK || got.Body.String() != "面板数据" {
		t.Fatalf("owner/admin 该能访问: %d %q", got.Code, got.Body.String())
	}
}

// 后台门不区分渠道: 会员身份只要带 owner/admin 角色也能进（后台 = 前台 + 权限）。
func TestAdminGateIsRoleBased(t *testing.T) {
	site := adminSite(t)
	id, err := site.Engine().RegisterAuth(nil, "member", "email", "boss@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "甲", "roles": []any{types.RoleOwner}}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "frontend", id)
	if err != nil {
		t.Fatal(err)
	}
	got := do(t, site, http.MethodGet, "/admin/panels", withBearer(token))
	if got.Code != http.StatusOK {
		t.Fatalf("会员带 owner 角色该能进后台: %d %q", got.Code, got.Body.String())
	}
}
