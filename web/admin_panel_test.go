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

// 站点面板的最小样子 —— 一个"内容审核"页: 列出草稿 + 批量发布。
//
// 组件源码是站点自己的文件（生产上放站点目录里, 用 panelFile 之类的 handler 读出来）;
// 这里直接内联一个, 好让这条链路（菜单 → 组件 → 端点）在测试里真的跑起来。
const examplePanelVue = `<template>
    <div class="panel-review">
        <h3>待审内容（{{ items.length }}）</h3>
        <el-table :data="items">
            <el-table-column label="标题" prop="fields.title" />
            <el-table-column label="操作"><el-button link @click="publish(row)">发布</el-button></el-table-column>
        </el-table>
    </div>
</template>
<script>
export default {
    name: 'ReviewPanel',
    data() { return { items: [] } },
    async mounted() { this.items = (await window.$api.get('/admin/review/pending')).items },
    methods: {
        async publish(row) {
            // 面板自己的写也走**受管入口** ⇒ 策略照旧生效（不是"后台特权通道"）
            await window.$api.post('/admin/review/publish', { id: row.id, revision: row.revision })
            this.items = (await window.$api.get('/admin/review/pending')).items
        },
    },
}
</script>
`

// panelSite 装一个站点面板的站点（面板 = 菜单一行 + 认证组里的端点与组件）。
func panelSite(t *testing.T) *Site {
	t.Helper()
	site := newPolicySite(t)
	site.Auth().Register(AuthRealm{Name: "staff", NodeType: "staff"}) // 后台身份走员工渠道
	site.Type("article").OnRead(func(c *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	site.Type("article").OnUpdate(func(_ *CmsCtx, _ *core.Node, _ *core.NodePatch, allow *Grant) error {
		allow.Add(types.RolePublic, "state")
		return nil
	})
	site.Type("article").OnDelete(func(_ *CmsCtx, _ *core.Node) error { return nil })

	// ── ① 菜单: HookAdminPanel（每次 /admin/panels 都 Fire ⇒ 可以按库里的状态出现）──
	site.Hook(HookAdminPanel, func(ctx *CmsCtx, panels *core.List[AdminPanel]) error {
		pending, err := sListPending(ctx)
		if err != nil {
			return err
		}
		// 响应式: 没有草稿就不显示这一项（真的查了一遍库）
		if pending == 0 {
			return nil
		}
		panels.Append(AdminPanel{
			Path:  "/review",
			Title: "内容审核（" + itoa(pending) + "）",
			Vue:   "/admin/review/panel.vue",
		})
		return nil
	})

	// ── ② 端点 + 组件: HookAdminMount（拿到的是**过了后台门**的组）──
	site.Hook(HookAdminMount, func(g *cho.Cho[*CmsCtx]) error {
		g.Get("/review/pending", func(ctx *CmsCtx) {
			items, total, err := ctx.List(core.NodeQuery{Type: "article", Where: so.P("=", "$state", "draft")}, 0, 0)
			if err != nil {
				ctx.Fail(err)
				return
			}
			_ = ctx.Json(http.StatusOK, map[string]any{"items": items, "total": total})
		})
		g.Post("/review/publish", func(ctx *CmsCtx) {
			var input struct {
				ID       int64 `json:"id"`
				Revision int64 `json:"revision"`
			}
			err := ctx.BindJSON(&input)
			if err != nil {
				ctx.Fail(err)
				return
			}
			// 改也走受管入口（策略仍生效）, 不是因为"在后台"就有特权
			_, err = ctx.Update("article", input.ID, &core.NodePatch{
				Revision: &input.Revision, Fields: core.Fields{"state": "published"}})
			if err != nil {
				ctx.Fail(err)
				return
			}
			_ = ctx.NoContent(http.StatusNoContent)
		})
		// 组件源码**也在认证组里**（面板代码不该公开）
		g.Get("/review/panel.vue", func(ctx *CmsCtx) {
			ctx.SetHeader("Content-Type", "text/plain; charset=utf-8")
			_ = ctx.String(http.StatusOK, examplePanelVue)
		})
		return nil
	})
	return site
}

// sListPending 面板菜单里查"待审数"（面板钩子拿得到 CmsCtx ⇒ 用受管读入口）。
func sListPending(ctx *CmsCtx) (int64, error) {
	_, total, err := ctx.List(core.NodeQuery{Type: "article", Where: so.P("=", "$state", "draft")}, 1, 0)
	return total, err
}

// 三块拼起来: 菜单出现（响应式）→ 组件与端点都在后台门后面 → 面板的写走受管入口。
func TestSitePanelEndToEnd(t *testing.T) {
	site := panelSite(t)
	owner, _ := adminToken(t, site, types.RoleOwner)
	anon := ""

	// 还没有草稿: 菜单里不该有这一项（钩子真的查了库）
	var menu struct {
		Items []AdminPanel `json:"items"`
	}
	got := do(t, site, http.MethodGet, "/admin/panels", withBearer(owner))
	if got.Code != http.StatusOK {
		t.Fatalf("panels = %d", got.Code)
	}
	if err := json.Unmarshal(got.Body.Bytes(), &menu); err != nil {
		t.Fatal(err)
	}
	if len(menu.Items) != 0 {
		t.Fatalf("没有草稿时不该出现审核面板: %#v", menu.Items)
	}

	// 造一篇草稿（系统写）
	id, err := site.Engine().CreateNode(nil, &core.Node{Type: "article",
		Fields: core.Fields{"title": "待审", "state": "draft"}})
	if err != nil {
		t.Fatal(err)
	}

	got = do(t, site, http.MethodGet, "/admin/panels", withBearer(owner))
	if err := json.Unmarshal(got.Body.Bytes(), &menu); err != nil {
		t.Fatal(err)
	}
	if len(menu.Items) != 1 {
		t.Fatalf("草稿出现后该有审核面板: %#v", menu.Items)
	}
	panel := menu.Items[0]
	if panel.Path != "/review" || panel.Vue != "/admin/review/panel.vue" {
		t.Fatalf("面板项 = %#v（前端 loadPanels 要 path 与 vue）", panel)
	}
	if !strings.Contains(panel.Title, "1") {
		t.Fatalf("标题该带待审数（响应式）: %q", panel.Title)
	}

	// 组件与端点都在门后面: 匿名拿不到（前端拿不到源码就渲染不出来, 这是有意的）
	for _, target := range []string{"/admin/review/panel.vue", "/admin/review/pending"} {
		if got := do(t, site, http.MethodGet, target, withBearer(anon)); got.Code != http.StatusUnauthorized {
			t.Fatalf("%s 匿名 = %d（面板代码不该公开）", target, got.Code)
		}
	}
	// 有后台角色但不是 owner/admin ⇒ 403（门是角色判定）
	staff, _ := adminToken(t, site)
	if got := do(t, site, http.MethodGet, "/admin/review/panel.vue", withBearer(staff)); got.Code != http.StatusForbidden {
		t.Fatalf("无后台角色 = %d", got.Code)
	}

	// 管理员: 拿得到组件源码（前端要能编译它）与端点数据
	got = do(t, site, http.MethodGet, "/admin/review/panel.vue", withBearer(owner))
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "ReviewPanel") {
		t.Fatalf("组件 = %d %q", got.Code, got.Body.String())
	}
	got = do(t, site, http.MethodGet, "/admin/review/pending", withBearer(owner))
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "待审") {
		t.Fatalf("待审列表 = %d %q", got.Code, got.Body.String())
	}

	// 面板的写走受管入口: 发布成功（策略放行了 state 字段）
	body := `{"id":` + itoa(id) + `,"revision":1}`
	got = jsonDo(t, site, http.MethodPost, "/admin/review/publish", body, withBearer(owner))
	if got.Code != http.StatusNoContent {
		t.Fatalf("发布 = %d %q", got.Code, got.Body.String())
	}
	updated, err := site.Engine().GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Fields.Str("state") != "published" {
		t.Fatalf("该真的发布了: %#v", updated.Fields)
	}
	// 发布之后菜单里的待审项消失（响应式钩子的意义）
	got = do(t, site, http.MethodGet, "/admin/panels", withBearer(owner))
	if err := json.Unmarshal(got.Body.Bytes(), &menu); err != nil {
		t.Fatal(err)
	}
	if len(menu.Items) != 0 {
		t.Fatalf("审完不该还有待审项: %#v", menu.Items)
	}
}

// 面板的写不是特权通道 —— 而且这里顺带钉住 "admin 只是能进后台的标识, 不是对 node
// 操作的角色": admin 进得来后台, 但字段权限一个没有（owner 才有 `*` 通配）,
// 于是策略没放行的字段照样 422。
func TestSitePanelWriteStillSubjectToPolicy(t *testing.T) {
	site := newPolicySite(t)
	site.Auth().Register(AuthRealm{Name: "staff", NodeType: "staff"})
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	// 更新规则只放 title（不给 state）
	site.Type("article").OnUpdate(func(_ *CmsCtx, _ *core.Node, _ *core.NodePatch, allow *Grant) error {
		allow.Add(types.RolePublic, "title")
		return nil
	})
	site.Hook(HookAdminMount, func(g *cho.Cho[*CmsCtx]) error {
		g.Post("/publish-anyway", func(ctx *CmsCtx) {
			var input struct {
				ID       int64 `json:"id"`
				Revision int64 `json:"revision"`
			}
			err := ctx.BindJSON(&input)
			if err != nil {
				ctx.Fail(err)
				return
			}
			_, err = ctx.Update("article", input.ID, &core.NodePatch{
				Revision: &input.Revision, Fields: core.Fields{"state": "published"}})
			if err != nil {
				ctx.Fail(err)
				return
			}
			_ = ctx.NoContent(http.StatusNoContent)
		})
		return nil
	})
	id, err := site.Engine().CreateNode(nil, &core.Node{Type: "article",
		Fields: core.Fields{"title": "甲", "state": "draft"}})
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := adminToken(t, site, types.RoleAdmin)
	body := `{"id":` + itoa(id) + `,"revision":1}`
	got := jsonDo(t, site, http.MethodPost, "/admin/publish-anyway", body, withBearer(admin))
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("策略没放行的字段该 422（admin 没有字段权限）: %d %q", got.Code, got.Body.String())
	}
	stored, err := site.Engine().GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Fields.Str("state") != "draft" {
		t.Fatalf("不该改到: %#v", stored.Fields)
	}
}
