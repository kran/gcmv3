package web

import (
	"slices"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// factsSite 一个"审核态影响可写性"的站点 —— 正是"规则按 node 属性定可写"的场景。
//
//	article: state ∈ {draft, published}; author 指向 member; phone 是敏感字段
//	读: 匿名看不到 phone; 会员看得到自己的草稿 + 已发布
//	写: 作者能改 title; **草稿期**还能改 state; 一旦发布, 只有 owner 能再改
func allowArticleCreate(site *Site) {
	site.Type("article").OnCreate(func(c *CmsCtx, node *core.Node, allow *Grant) error {
		if c.Actor().IsAnonymous() {
			return Unauthorized("请先登录")
		}
		allow.Add(types.RolePublic, "title", "state", "phone")
		node.Fields["author"] = c.Actor().NodeID
		return nil
	})
}

func factsSite(t *testing.T) (*Site, *CmsCtx) {
	t.Helper()
	site := newPolicySite(t)
	allowArticleCreate(site)
	site.Type("article").OnRead(func(c *CmsCtx, where *so.Where, hide *Grant) error {
		if c.Actor().IsOwner() {
			*where = so.P("true")
			return nil
		}
		published := so.P("=", "$state", "published")
		if c.Actor().IsAnonymous() {
			hide.Add(types.RolePublic, "phone")
			*where = published
			return nil
		}
		*where = so.OR(published, so.P("ref", "->author", so.P("=", "id", c.Actor().NodeID)))
		if c.Actor().IsAnonymous() {
			hide.Add(types.RolePublic, "phone")
		}
		return nil
	})
	site.Type("article").OnUpdate(func(c *CmsCtx, node *core.Node, patch *core.NodePatch, allow *Grant) error {
		author, _ := node.Fields["author"].(int64)
		isAuthor := author == c.Actor().NodeID
		published := node.Fields.Str("state") == "published"
		switch {
		case c.Actor().IsOwner():
			allow.Add(types.RolePublic, "title", "state", "phone")
		case isAuthor && !published:
			// 草稿期: 作者能改标题与状态（"根据 node 属性定可写"）
			allow.Add(types.RolePublic, "title", "state")
		case isAuthor:
			// 已发布: 只能改标题
			allow.Add(types.RolePublic, "title")
		default:
			return Forbidden("不是作者")
		}
		return nil
	})
	return site, memberCtx(t, site)
}

func factsArticle(t *testing.T, c *CmsCtx, fields core.Fields) *core.Node {
	t.Helper()
	node, err := c.Create("article", fields)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

// 创建/更新响应里带 masked 与 editable。
func TestFactsOnWriteResponses(t *testing.T) {
	_, me := factsSite(t)
	created := factsArticle(t, me, core.Fields{"title": "草稿"})
	if strings.Join(created.Editable, ",") != "title,state" {
		t.Fatalf("草稿期可写 = %#v（author 由规则补, phone 没授予）", created.Editable)
	}
	if created.Masked != nil {
		t.Fatalf("作者看不到自己被裁的字段: %#v", created.Masked)
	}
	// 发布之后: editable 立刻只剩 title（用**这次提交的 patch** 求值）
	_, err := me.Update("article", created.ID, &core.NodePatch{
		Revision: ptrInt64(1), Fields: core.Fields{"state": "published"}})
	// 规则允许作者在草稿期改 state ⇒ 这次能过
	if err != nil {
		t.Fatal(err)
	}
	after, err := me.Get("article", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(after.Editable, ",") != "title" {
		t.Fatalf("发布后该只剩 title: %#v", after.Editable)
	}
}

// **不变式**: editable ∩ masked = ∅（可写 ⊆ 可读）。
func TestFactsEditableIsReadable(t *testing.T) {
	site, me := factsSite(t)
	created := factsArticle(t, me, core.Fields{"title": "草稿"})
	// 让 phone 有值, 然后匿名读（phone 被裁）
	err := site.Engine().PatchNode(nil, created.ID, &core.NodePatch{
		Revision: ptrInt64(1), Fields: core.Fields{"state": "published", "phone": "138"}})
	if err != nil {
		t.Fatal(err)
	}
	anon, _ := ctxFor(site)
	node, err := anon.Get("article", created.ID)
	if err != nil || node == nil {
		t.Fatalf("匿名该看得到已发布: %#v %v", node, err)
	}
	if strings.Join(node.Masked, ",") != "phone" {
		t.Fatalf("masked = %#v", node.Masked)
	}
	for _, field := range node.Editable {
		if slices.Contains(node.Masked, field) {
			t.Fatalf("可写字段同时被掩码: %q（%#v / %#v）", field, node.Editable, node.Masked)
		}
	}
}

// 规则拿到的是**原始 node**（未掩码）—— 靠它判定的规则不能因为"这个字段对外隐藏"而失效。
func TestRulesSeeUnmaskedNode(t *testing.T) {
	site := newPolicySite(t)
	allowArticleCreate(site)
	site.Type("article").OnRead(func(c *CmsCtx, where *so.Where, hide *Grant) error {
		*where = so.P("true")
		hide.Add(types.RolePublic, "phone") // 谁都看不到 phone
		return nil
	})
	seen := ""
	site.Type("article").OnUpdate(func(_ *CmsCtx, node *core.Node, _ *core.NodePatch, allow *Grant) error {
		seen = node.Fields.Str("phone")      // 规则读的是真值
		allow.Add(types.RolePublic, "title") // 但不授予 phone（否则违反可写 ⊆ 可读）
		return nil
	})
	cms := memberCtx(t, site)
	created, err := cms.Create("article", core.Fields{"title": "甲"})
	if err != nil {
		t.Fatal(err)
	}
	err = site.Engine().PatchNode(nil, created.ID, &core.NodePatch{
		Revision: ptrInt64(1), Fields: core.Fields{"phone": "139"}})
	if err != nil {
		t.Fatal(err)
	}
	// 读一次（会跑 OnUpdate 求 editable）⇒ 规则该看到真值
	_, err = cms.Get("article", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if seen != "139" {
		t.Fatalf("规则该看到未掩码的 node: %q", seen)
	}
}

// 列表不带这两个事实（逐项求值太贵）。
func TestFactsAbsentInList(t *testing.T) {
	_, me := factsSite(t)
	factsArticle(t, me, core.Fields{"title": "草稿"})
	items, _, err := me.List(core.NodeQuery{Type: "article"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("该有一条")
	}
	if items[0].Editable != nil || items[0].Masked != nil {
		t.Fatalf("列表不该带事实: %#v", items[0])
	}
}

// 规则拒绝更新时 editable 为空（不是"全都可写"）。
func TestFactsEmptyWhenUpdateDenied(t *testing.T) {
	site := newPolicySite(t)
	allowArticleCreate(site)
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	site.Type("article").OnUpdate(func(_ *CmsCtx, _ *core.Node, _ *core.NodePatch, _ *Grant) error {
		return Forbidden("谁都不能改")
	})
	cms := memberCtx(t, site)
	created, err := cms.Create("article", core.Fields{"title": "甲"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Editable != nil {
		t.Fatalf("被拒时该为空: %#v", created.Editable)
	}
}

// 没有 OnUpdate 规则的类型: 没有可写字段（默认拒绝）。
func TestFactsWithoutUpdateRule(t *testing.T) {
	site := newPolicySite(t)
	allowArticleCreate(site)
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	cms := memberCtx(t, site)
	created, err := cms.Create("article", core.Fields{"title": "甲"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Editable != nil {
		t.Fatalf("没注册更新规则该没有可写字段: %#v", created.Editable)
	}
	// 但 masked 是有的（读规则算出来的）
	if _, _, err := cms.readRule("article"); err != nil {
		t.Fatal(err)
	}
}

// 单节点读也带事实（不只是写响应）。
func TestFactsOnSingleRead(t *testing.T) {
	site, me := factsSite(t)
	created := factsArticle(t, me, core.Fields{"title": "草稿"})
	node, err := me.Get("article", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(node.Editable, ",") != "title,state" {
		t.Fatalf("single read 的 editable = %#v", node.Editable)
	}
	_ = site
}

// owner 通过 Grant 的 "*" 拿到所有声明字段（除被裁的）。
func TestFactsOwnerWildcard(t *testing.T) {
	site, _ := factsSite(t)
	owner, _ := ctxFor(site)
	owner.SetActor(Actor{NodeID: 1, NodeType: "member", Realm: "frontend",
		Roles: []string{types.RoleOwner}})
	created, err := site.Engine().CreateNode(nil, &core.Node{Type: "article",
		Fields: core.Fields{"title": "甲", "state": "published"}})
	if err != nil {
		t.Fatal(err)
	}
	node, err := owner.Get("article", created)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"title", "state", "phone", "author"} {
		if !slices.Contains(node.Editable, want) {
			t.Fatalf("owner 该能写 %q: %#v", want, node.Editable)
		}
	}
}
