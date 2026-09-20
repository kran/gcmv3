package web

import (
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	so "github.com/kran/gcmv3/so"
)

// labelSite 一个"读规则全放行 + 按需掩码"的站点（article 声明了 admin.display: title）。
func labelSite(t *testing.T, hide ...string) *CmsCtx {
	t.Helper()
	site := newPolicySite(t)
	allowArticleCreate(site)
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, masked *Grant) error {
		*where = so.P("true")
		for _, name := range hide {
			masked.Add("public", name)
		}
		return nil
	})
	// member 也得能读: 目标不可读时展开的整条引用不下发（fail-closed）,
	// 那样就测不到"展开出来的引用带不带显示名"了。
	site.Type("member").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	return memberCtx(t, site)
}

// 显示名 = 类型声明 admin.display 的值 —— 读/写/列表三条路都带（前端读 extra.display,
// 不自己猜字段名: 猜就有第二份真相）。
func TestLabelFromDeclaredDisplay(t *testing.T) {
	cms := labelSite(t)
	created, err := cms.Create("article", core.Fields{"title": "协会动态"})
	if err != nil {
		t.Fatal(err)
	}
	if got := created.Extra["display"]; got != "协会动态" {
		t.Fatalf("写响应该带显示名: %#v", created.Extra)
	}
	one, err := cms.Get("article", created.ID)
	if err != nil || one == nil {
		t.Fatalf("get: %v %v", one, err)
	}
	if got := one.Extra["display"]; got != "协会动态" {
		t.Fatalf("详情该带显示名: %#v", one.Extra)
	}
	items, _, err := cms.List(core.NodeQuery{Type: "article"}, 0, 0)
	if err != nil || len(items) == 0 {
		t.Fatalf("list: %v %v", items, err)
	}
	if got := items[0].Extra["display"]; got != "协会动态" {
		t.Fatalf("列表也该带显示名: %#v", items[0].Extra)
	}
}

// 登录 / /api/auth/me 那条路也要有显示名 —— 它们走的是 MaskNode（同一个关口）,
// 不是读入口。用户卡片上写"#12"而不是名字就是这条漏了。
func TestLabelOnAuthMePath(t *testing.T) {
	cms := labelSite(t)
	// /api/auth/me 的实现就是 MaskNode(user) —— 这里直接验它
	user, err := cms.Principal()
	if err != nil {
		t.Fatal(err)
	}
	masked, err := cms.MaskNode(user)
	if err != nil {
		t.Fatal(err)
	}
	if masked.Extra["display"] == nil {
		t.Fatalf("MaskNode 该补显示名（登录/me 都走它）: %#v", masked.Extra)
	}
}

// **掩码不能从标签漏出去**: admin.display 指向的字段自己不可读时, 顺着往下走
// （columns → 字段序 → #id）, 绝不回头去读被裁掉的真值。
func TestLabelDoesNotLeakMaskedDisplay(t *testing.T) {
	cms := labelSite(t, "title") // 把显示名字段裁掉
	// 引擎直接建（= 系统自己写）: 以"读不到 title"的身份去 Create 会先被
	// 可写 ⊆ 可读 挡住（"对当前身份不可见"）, 那是另一条规则、另一个测试。
	created, err := cms.site.Engine().CreateNode(nil, &core.Node{
		Type: "article", Fields: core.Fields{"title": "不该泄漏的标题", "state": "draft"},
	})
	if err != nil {
		t.Fatal(err)
	}
	one, err := cms.Get("article", created)
	if err != nil || one == nil {
		t.Fatalf("get: %v %v", one, err)
	}
	if _, ok := one.Fields["title"]; ok {
		t.Fatal("前提: title 该被裁掉")
	}
	label, _ := one.Extra["display"].(string)
	if strings.Contains(label, "不该泄漏的标题") {
		t.Fatalf("显示名泄漏了被裁掉的字段值: %q", label)
	}
	// 声明里 columns 退到 state（默认 draft）—— 有值就用它, 总比 #id 有用
	if label != "draft" {
		t.Fatalf("该退到 columns 里第一个可见且有值的（state）: %q", label)
	}
}

// 可见字段全空 ⇒ `#id`（不猜、也不空手）。
func TestLabelFallsBackToID(t *testing.T) {
	cms := labelSite(t, "title", "state") // 声明里只有 title/state, 两个都裁掉
	created, err := cms.site.Engine().CreateNode(nil, &core.Node{
		Type: "article", Fields: core.Fields{"title": "都被裁掉了"},
	})
	if err != nil {
		t.Fatal(err)
	}
	one, err := cms.Get("article", created)
	if err != nil || one == nil {
		t.Fatalf("get: %v %v", one, err)
	}
	label, _ := one.Extra["display"].(string)
	if !strings.HasPrefix(label, "#") {
		t.Fatalf("没有任何可见字段有值时该退到 #id: %q（fields=%#v）", label, one.Fields)
	}
}

// 展开出来的引用目标也要显示名 —— 前台拿 expand 渲染"行业/地区/作者"标签。
func TestLabelOnExpandedRefs(t *testing.T) {
	cms := labelSite(t)
	author, err := cms.site.Engine().CreateNode(nil, &core.Node{
		Type: "member", Fields: core.Fields{"name": "张三"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// author 不在会员可写字段里（规则只授了 title/state）⇒ 引擎直接建
	created, err := cms.site.Engine().CreateNode(nil, &core.Node{
		Type: "article", Fields: core.Fields{"title": "带作者的文章", "author": author},
	})
	if err != nil {
		t.Fatal(err)
	}
	one, err := cms.Get("article", created)
	if err != nil || one == nil {
		t.Fatalf("get: %v %v", one, err)
	}
	ref, ok := one.Expand["author"].(*core.Node)
	if !ok {
		t.Fatalf("该展开出作者: %#v", one.Expand)
	}
	// member 没声明 admin.display ⇒ 退到字段序里第一个有值的（name）
	if got := ref.Extra["display"]; got != "张三" {
		t.Fatalf("展开出来的引用也该带显示名: %#v", ref.Extra)
	}
}
