package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// 会员自助内容站的策略: 管理看全部, 会员看"已发布 + 自己的", 匿名只看已发布;
// 写: 只作者本人能改删, 且只能改 title。
func memberSite(t *testing.T) (*Site, *CmsCtx, *CmsCtx) {
	t.Helper()
	site := newPolicySite(t)
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
		*where = so.OR(published,
			so.P("ref", "->author", so.P("=", "id", c.Actor().NodeID)))
		return nil
	})
	site.Type("article").OnCreate(func(c *CmsCtx, node *core.Node, allow *Grant) error {
		if c.Actor().IsAnonymous() {
			return Unauthorized("请先登录")
		}
		allow.Add(types.RolePublic, "title")
		node.Fields["author"] = c.Actor().NodeID // 规则自己补的字段不参与白名单
		return nil
	})
	site.Type("article").OnUpdate(func(c *CmsCtx, id int64, _ *core.NodePatch, allow *Grant) error {
		existing, err := c.Engine().GetNode(id)
		if err != nil || existing == nil {
			return NotFound("不存在")
		}
		if !isAuthor(c, existing) {
			return Forbidden("仅作者本人可改")
		}
		allow.Add(types.RolePublic, "title")
		return nil
	})
	site.Type("article").OnDelete(func(c *CmsCtx, id int64) error {
		existing, err := c.Engine().GetNode(id)
		if err != nil || existing == nil {
			return NotFound("不存在")
		}
		if !isAuthor(c, existing) {
			return Forbidden("仅作者本人可删")
		}
		return nil
	})
	// 会员身份: 两个不同的会员
	me := memberCtx(t, site)
	otherID, err := site.Engine().RegisterAuth(nil, "member", "email", "b@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "乙"}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "frontend", otherID)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := ctxFor(site, withBearer(token))
	return site, me, other
}

func isAuthor(c *CmsCtx, node *core.Node) bool {
	author, ok := node.Fields["author"].(int64)
	return ok && author == c.Actor().NodeID
}

// createArticle 走真实写入口建一篇（作者 = 传进来的上下文）。
func createArticle(t *testing.T, c *CmsCtx, title string) *core.Node {
	t.Helper()
	node, err := c.Create("article", core.Fields{"title": title})
	if err != nil {
		t.Fatal(err)
	}
	return node
}

// 列表: 范围按身份收窄, 客户端条件只能更窄（放宽不了）。
func TestListScope(t *testing.T) {
	site, me, other := memberSite(t)
	mine := createArticle(t, me, "我的草稿")
	theirs := createArticle(t, other, "别人的草稿")

	// 作者自己看得到自己的草稿
	mineList, total, err := me.List(core.NodeQuery{Type: "article"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(mineList) != 1 || mineList[0].ID != mine.ID {
		t.Fatalf("会员该只看到自己的: total=%d %#v", total, mineList)
	}
	// 匿名看不到任何草稿
	anon, _ := ctxFor(site)
	anonList, total, err := anon.List(core.NodeQuery{Type: "article"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(anonList) != 0 {
		t.Fatalf("匿名不该看到草稿: total=%d", total)
	}
	// 客户端条件只能收窄: 显式点名别人的节点也拿不到
	list, total, err := me.List(core.NodeQuery{Type: "article", Where: so.P("=", "id", theirs.ID)}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(list) != 0 {
		t.Fatalf("范围外的行点不出来: total=%d %#v", total, list)
	}
	// 发布之后匿名看得到, 但 phone 被掩码（列表里）
	id := mine.ID
	_, err = me.Update("article", id, patchWithRevision(1, core.Fields{"title": "我的草稿"}))
	if err != nil {
		t.Fatal(err)
	}
	err = site.Engine().PatchNode(nil, id, patchWithRevision(2, core.Fields{"state": "published", "phone": "138"}))
	if err != nil {
		t.Fatal(err)
	}
	anonList, total, err = anon.List(core.NodeQuery{Type: "article"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(anonList) != 1 {
		t.Fatalf("发布后匿名该看到: total=%d", total)
	}
	if _, ok := anonList[0].Fields["phone"]; ok {
		t.Fatalf("phone 该被掩码: %#v", anonList[0].Fields)
	}
}

// Get: 不存在与不可见都返回 (nil, nil)（调用方分不出来）。
func TestGetInvisible(t *testing.T) {
	_, me, other := memberSite(t)
	theirs := createArticle(t, other, "别人的草稿")

	got, err := me.Get("article", theirs.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("范围外该 (nil, nil): %#v", got)
	}
	got, err = me.Get("article", int64(999999))
	if err != nil || got != nil {
		t.Fatalf("不存在该 (nil, nil): %#v %v", got, err)
	}
	// 作者本人拿得到
	mine := createArticle(t, me, "我的")
	got, err = me.Get("article", mine.ID)
	if err != nil || got == nil || got.ID != mine.ID {
		t.Fatalf("自己的该拿得到: %#v %v", got, err)
	}
}

// **看得到才改得到**: 范围外的行连改都改不了（站点规则没判归属也一样）。
func TestWriteRequiresVisible(t *testing.T) {
	site, me, other := memberSite(t)
	theirs := createArticle(t, other, "别人的草稿")

	// 会员 me 看不到 other 的草稿 ⇒ 404, 而不是"规则放行就改了"
	_, err := me.Update("article", theirs.ID, patchWithRevision(1, core.Fields{"title": "篡改"}))
	var notFound *Error
	if !errors.As(err, &notFound) || notFound.Status != http.StatusNotFound {
		t.Fatalf("err = %v", err)
	}
	err = me.Delete("article", theirs.ID)
	if !errors.As(err, &notFound) || notFound.Status != http.StatusNotFound {
		t.Fatalf("err = %v", err)
	}
	// 确认真的没被改/删
	node, err := site.Engine().GetNode(theirs.ID)
	if err != nil || node == nil || node.Fields.Str("title") != "别人的草稿" {
		t.Fatalf("不该被改: %#v %v", node, err)
	}
}

// 自己的草稿改得动（读范围含自己的 ⇒ 闸门放行）, 但只能改白名单里的字段。
func TestUpdateOwnDraft(t *testing.T) {
	_, me, _ := memberSite(t)
	mine := createArticle(t, me, "我的")

	updated, err := me.Update("article", mine.ID, patchWithRevision(1, core.Fields{"title": "改过"}))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Fields.Str("title") != "改过" {
		t.Fatalf("该改上了: %#v", updated.Fields)
	}
	// 白名单外的字段 422
	_, err = me.Update("article", mine.ID, patchWithRevision(2, core.Fields{"state": "published"}))
	var invalid *Error
	if !errors.As(err, &invalid) || invalid.Status != http.StatusUnprocessableEntity {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(invalid.Details["state"], "不可写") {
		t.Fatalf("details = %#v", invalid.Details)
	}
}

// 乐观锁: revision 缺了/过期了都拒绝（不替客户端补最新版本）。
func TestUpdateRevision(t *testing.T) {
	_, me, _ := memberSite(t)
	mine := createArticle(t, me, "我的")

	_, err := me.Update("article", mine.ID, &core.NodePatch{Fields: core.Fields{"title": "无版本"}})
	var invalid *Error
	if !errors.As(err, &invalid) || invalid.Status != http.StatusUnprocessableEntity {
		t.Fatalf("缺 revision 的 err = %v", err)
	}
	_, err = me.Update("article", mine.ID, patchWithRevision(99, core.Fields{"title": "过期"}))
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict {
		t.Fatalf("过期 revision 的 err = %v", err)
	}
	if conflict.Code != "revision_conflict" {
		t.Fatalf("code = %q", conflict.Code)
	}
}

// 创建: 匿名 401; 规则补的 author 落了库; 返回的节点是掩码过的。
func TestCreatePolicy(t *testing.T) {
	site, me, _ := memberSite(t)
	anon, _ := ctxFor(site)
	_, err := anon.Create("article", core.Fields{"title": "匿名"})
	var denied *Error
	if !errors.As(err, &denied) || denied.Status == http.StatusOK {
		t.Fatalf("匿名的 err = %v", err)
	}

	created := createArticle(t, me, "标题")
	if created.Fields["author"] != me.Actor().NodeID {
		t.Fatalf("规则补的 author 该落库: %#v", created.Fields)
	}
	if created.Fields.Str("state") != "draft" {
		t.Fatalf("默认值该由引擎补上: %#v", created.Fields)
	}
	stored, err := site.Engine().GetNode(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Fields["author"] != me.Actor().NodeID {
		t.Fatalf("库里该有 author: %#v", stored.Fields)
	}
}

// 删除: 作者本人可以; 被引用时 409。
func TestDeletePolicy(t *testing.T) {
	site, me, other := memberSite(t)
	mine := createArticle(t, me, "我的")
	theirs := createArticle(t, other, "别人的")

	err := me.Delete("article", theirs.ID)
	var notFound *Error
	if !errors.As(err, &notFound) || notFound.Status != http.StatusNotFound {
		t.Fatalf("别人的该 404: %v", err)
	}
	err = me.Delete("article", mine.ID)
	if err != nil {
		t.Fatalf("自己的该删得掉: %v", err)
	}
	gone, err := site.Engine().GetNode(mine.ID)
	if gone != nil || !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("该真删了: %#v %v", gone, err)
	}
	// 通过读入口看: 也是 (nil, nil)
	got, err := me.Get("article", mine.ID)
	if err != nil || got != nil {
		t.Fatalf("删掉后该 (nil, nil): %#v %v", got, err)
	}
}

// 没注册读规则的类型: List 403（默认不开放）, 不是"返回全部"。
func TestListWithoutRule(t *testing.T) {
	site := newPolicySite(t)
	_, err := site.Engine().CreateNode(nil, &core.Node{Type: "staff", Fields: core.Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	cms, _ := ctxFor(site)
	_, _, err = cms.List(core.NodeQuery{Type: "staff"}, 0, 0)
	var denied *Error
	if !errors.As(err, &denied) || denied.Status != http.StatusForbidden {
		t.Fatalf("err = %v", err)
	}
}

// 写响应也过掩码 —— 否则写接口是掩码的旁路。
//
// 挡的不是"客户端写进去的字段"（可写 ⊆ 可读, 那种字段不可能同时不可见）,
// 而是**规则/系统补进去的、当前身份看不到的字段**。
func TestWriteResponseIsMasked(t *testing.T) {
	site := newPolicySite(t)
	site.Type("article").OnRead(func(c *CmsCtx, where *so.Where, hide *Grant) error {
		*where = so.P("true")
		if c.Actor().IsAnonymous() {
			hide.Add(types.RolePublic, "phone")
		}
		return nil
	})
	site.Type("article").OnCreate(func(_ *CmsCtx, node *core.Node, allow *Grant) error {
		allow.Add(types.RolePublic, "title")
		// 规则自己补一个匿名看不到的字段（真实场景: 从请求上下文/上游补值）
		node.Fields["phone"] = "138"
		return nil
	})
	cms, _ := ctxFor(site)
	created, err := cms.Create("article", core.Fields{"title": "甲"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := created.Fields["phone"]; ok {
		t.Fatalf("写响应该掩码: %#v", created.Fields)
	}
	if created.Fields.Str("title") != "甲" {
		t.Fatalf("其它字段该在: %#v", created.Fields)
	}
	// 库里该真写上了（掩码只影响输出）
	stored, err := site.Engine().GetNode(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Fields["phone"] != "138" {
		t.Fatalf("库里该有: %#v", stored.Fields)
	}
	// 同一个节点, 换成看得到 phone 的身份读 ⇒ 没被裁
	cms.SetActor(Actor{NodeID: 1, NodeType: "member", Realm: "frontend"})
	got, err := cms.Get("article", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fields["phone"] != "138" {
		t.Fatalf("别的身份该看得到: %#v", got.Fields)
	}
}

func patchWithRevision(revision int64, fields core.Fields) *core.NodePatch {
	return &core.NodePatch{Revision: &revision, Fields: fields}
}
