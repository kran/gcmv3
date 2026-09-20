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

// 策略测试用的类型: 有地址的公开内容 + 会员（带角色词表）+ 只给管理看的员工。
const policyTypesYAML = `
types:
  article:
    capabilities: { addressable: true }
    fields:
      - { name: title, kind: text }
      - { name: state, kind: select, options: [draft, published], default: draft }
      - { name: phone, kind: text }
      - { name: author, kind: ref, to: member }
  member:
    capabilities:
      authentication: { roles: [editor, 秘书处] }
    fields:
      - { name: name, kind: text }
      - { name: phone, kind: text }
      # 真实站点有一模一样的"审核状态"字段（上传规则之类会读它）
      - { name: approval_state, kind: select, options: [pending, approved], default: pending }
  staff:
    fields:
      - { name: name, kind: text }
`

func newPolicySite(t *testing.T) *Site {
	t.Helper()
	basedir := t.TempDir()
	err := writeTypesFile(basedir, policyTypesYAML)
	if err != nil {
		t.Fatal(err)
	}
	site, err := Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = site.Close() })
	// 认证渠道: 各测试用 CreateSession(realm="frontend") 造身份 ⇒ 得先声明出来
	//（未声明的渠道会被 Actor() 拒掉 —— 这条本身也有测试）。
	site.Auth().Register(AuthRealm{Name: "frontend", NodeType: "member", Default: true})
	return site
}

// 会员节点 + 会话 ⇒ 已认证上下文。roles 是词表字段的值。
func memberCtx(t *testing.T, site *Site, roles ...string) *CmsCtx {
	t.Helper()
	nodeRoles := make([]any, 0, len(roles))
	for _, role := range roles {
		nodeRoles = append(nodeRoles, role)
	}
	id, err := site.Engine().RegisterAuth(nil, "member", "email", "a@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "甲", "roles": nodeRoles}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "frontend", id)
	if err != nil {
		t.Fatal(err)
	}
	cms, _ := ctxFor(site, withBearer(token))
	return cms
}

// 没注册读规则 ⇒ 读不出来（默认拒绝, 不是默认全公开）。
func TestReadRuleDefaultDenies(t *testing.T) {
	site := newPolicySite(t)
	cms, _ := ctxFor(site)
	_, _, err := cms.readRule("article")
	var denied *Error
	if !errors.As(err, &denied) || denied.Status != http.StatusForbidden {
		t.Fatalf("err = %v", err)
	}
}

// 规则跑完没给行范围 ⇒ 配置错误（零值 ≠ "全不限"）。
func TestReadRuleRequiresScope(t *testing.T) {
	site := newPolicySite(t)
	site.Type("article").OnRead(func(_ *CmsCtx, _ *so.Where, _ *Grant) error { return nil })
	cms, _ := ctxFor(site)
	_, _, err := cms.readRule("article")
	if err == nil || !strings.Contains(err.Error(), "produced no scope") {
		t.Fatalf("err = %v", err)
	}
}

// 拼错字段名 / 角色名 ⇒ 报错（静默无效 = 泄漏或假放行）。
func TestReadRuleValidatesGrant(t *testing.T) {
	cases := []struct {
		name  string
		rule  ReadRule
		match string
	}{
		{
			"字段拼错",
			func(_ *CmsCtx, where *so.Where, hide *Grant) error {
				*where = so.P("true")
				hide.Add(types.RolePublic, "titel")
				return nil
			},
			"undeclared field",
		},
		{
			"角色拼错",
			func(_ *CmsCtx, where *so.Where, hide *Grant) error {
				*where = so.P("true")
				hide.Add("membr", "phone")
				return nil
			},
			"unknown role",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			site := newPolicySite(t)
			site.Type("article").OnRead(test.rule)
			cms, _ := ctxFor(site)
			_, _, err := cms.readRule("article")
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("err = %v, want 含 %q", err, test.match)
			}
		})
	}
}

// 角色词汇是**站点级**的: 别的类型的角色名与词表都认（打错字仍然拦）。
func TestValidRoleVocabulary(t *testing.T) {
	site := newPolicySite(t)
	cases := map[string]bool{
		types.RoleOwner:  true,
		types.RoleAdmin:  true,
		types.RolePublic: true,
		"article":        true,  // 类型名
		"staff":          true,  // 别的类型名
		"editor":         true,  // member 的 authentication 词表
		"秘书处":            true,  // 同上
		"nobody":         false, // 没这个角色
		"":               false,
	}
	for role, want := range cases {
		if got := site.validRole(role); got != want {
			t.Fatalf("validRole(%q) = %v, want %v", role, got, want)
		}
	}
}

// 读规则: 行范围 + 按角色算隐藏字段（声明顺序, 输出稳定）。
func TestReadRuleScopeAndHide(t *testing.T) {
	site := newPolicySite(t)
	site.Type("article").OnRead(func(c *CmsCtx, where *so.Where, hide *Grant) error {
		if c.Actor().IsOwner() {
			*where = so.P("true")
			return nil
		}
		*where = so.P("=", "$state", "published")
		if c.Actor().IsAnonymous() {
			hide.Add(types.RolePublic, "phone")
		}
		return nil
	})

	// 匿名: 范围 = 已发布, 藏 phone
	cms, _ := ctxFor(site)
	where, hidden, err := cms.readRule("article")
	if err != nil {
		t.Fatal(err)
	}
	if where.IsZero() {
		t.Fatal("范围该有值")
	}
	if strings.Join(hidden, ",") != "phone" {
		t.Fatalf("hidden = %#v", hidden)
	}
	// 真机: 这个范围真的收窄了行（把解析出的 where 交给引擎 —— D2 的读入口做的事）
	eng := cms.Engine()
	id, err := eng.CreateNode(nil, &core.Node{Type: "article",
		Fields: core.Fields{"title": "草稿", "phone": "138", "state": "draft"}})
	if err != nil {
		t.Fatal(err)
	}
	count, err := eng.CountNodes(core.NodeQuery{Type: "article", Where: where}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("匿名不该数到草稿: %d", count)
	}
	err = eng.PatchNode(nil, id, &core.NodePatch{
		Revision: ptrInt64(1), Fields: core.Fields{"state": "published"}})
	if err != nil {
		t.Fatal(err)
	}
	count, err = eng.CountNodes(core.NodeQuery{Type: "article", Where: where}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("发布后该数得到: %d", count)
	}
	// 掩码: 同一份节点过 MaskNode, phone 该消失（真数据, 不是造的结构体）
	nodes, err := eng.GetNodes(core.NodeQuery{Type: "article", Where: where}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	masked, err := cms.MaskNode(nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := masked.Fields["phone"]; ok {
		t.Fatalf("phone 该被掩码: %#v", masked.Fields)
	}
	if masked.Fields["title"] != "草稿" {
		t.Fatalf("别的字段该原样: %#v", masked.Fields)
	}
	if _, ok := nodes[0].Fields["phone"]; !ok {
		t.Fatal("MaskNode 不该改入参")
	}
}

// 每请求每类型只求一次; 换身份（SetActor）作废缓存。
func TestReadRuleCachedPerRequest(t *testing.T) {
	site := newPolicySite(t)
	calls := 0
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		calls++
		*where = so.P("true")
		return nil
	})
	cms, _ := ctxFor(site)
	if _, _, err := cms.readRule("article"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cms.readRule("article"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("同一请求内该只求一次, calls = %d", calls)
	}
	// 换身份 ⇒ 结果作废（读规则是当前 Actor 的函数）
	cms.SetActor(Actor{NodeID: 7, NodeType: "member", Realm: "frontend"})
	if _, _, err := cms.readRule("article"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("换身份后该重算, calls = %d", calls)
	}
}

// 写侧: 默认拒绝（匿名 401 / 已认证 403）。
func TestWriteDefaultDenies(t *testing.T) {
	site := newPolicySite(t)
	cms, _ := ctxFor(site)
	err := cms.authorizeCreate(&core.Node{Type: "article", Fields: core.Fields{"title": "甲"}})
	var denied *Error
	if !errors.As(err, &denied) || denied.Status != http.StatusUnauthorized {
		t.Fatalf("匿名的 err = %v", err)
	}
	authed := memberCtx(t, site)
	err = authed.authorizeCreate(&core.Node{Type: "article", Fields: core.Fields{"title": "甲"}})
	if !errors.As(err, &denied) || denied.Status != http.StatusForbidden {
		t.Fatalf("已认证的 err = %v", err)
	}
}

// 写侧白名单: 规则授予的字段之外一律 422; 规则自己补的字段不算客户端提交。
func TestAuthorizeCreateWhitelist(t *testing.T) {
	site := newPolicySite(t)
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	site.Type("article").OnCreate(func(c *CmsCtx, node *core.Node, allow *Grant) error {
		// 客户端只能提交 title; author 由规则自己补
		allow.Add(types.RolePublic, "title")
		node.Fields["author"] = c.Actor().NodeID
		delete(node.Fields, "state")
		return nil
	})
	cms := memberCtx(t, site)

	node := &core.Node{Type: "article", Fields: core.Fields{"title": "甲", "state": "published"}}
	err := cms.authorizeCreate(node)
	var invalid *Error
	if !errors.As(err, &invalid) || invalid.Status != http.StatusUnprocessableEntity {
		t.Fatalf("err = %v", err)
	}
	if invalid.Details["state"] == "" {
		t.Fatalf("该指出是哪个字段: %#v", invalid.Details)
	}
	// 规则补的 author 不是客户端提交的 ⇒ 不参与白名单
	if _, ok := invalid.Details["author"]; ok {
		t.Fatalf("规则自己补的字段不该被白名单拦: %#v", invalid.Details)
	}
}

// 可写 ⊆ 可读: 读规则藏起来的字段, 写也不行（否则能"写进去再读出来"绕过掩码）。
func TestWritableImpliesReadable(t *testing.T) {
	site := newPolicySite(t)
	site.Type("member").OnRead(func(c *CmsCtx, where *so.Where, hide *Grant) error {
		*where = so.P("true")
		if !c.Actor().IsOwner() {
			hide.Add(types.RolePublic, "phone")
		}
		return nil
	})
	site.Type("member").OnUpdate(func(_ *CmsCtx, _ *core.Node, _ *core.NodePatch, allow *Grant) error {
		allow.Add(types.RolePublic, "phone", "name")
		return nil
	})
	cms := memberCtx(t, site, "editor")
	node := &core.Node{ID: 1, Type: "member"}
	err := cms.authorizeUpdate(node, &core.NodePatch{Fields: core.Fields{"phone": "138", "name": "乙"}})
	var invalid *Error
	if !errors.As(err, &invalid) || invalid.Status != http.StatusUnprocessableEntity {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(invalid.Details["phone"], "不可见") {
		t.Fatalf("该说不可见: %#v", invalid.Details)
	}
	if _, ok := invalid.Details["name"]; ok {
		t.Fatalf("可写的字段不该被拦: %#v", invalid.Details)
	}
}

// owner 在写侧默认被授予全部字段 —— 但它仍然受读规则约束（可写 ⊆ 可读）。
func TestOwnerWriteGrant(t *testing.T) {
	site := newPolicySite(t)
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	site.Type("article").OnUpdate(func(_ *CmsCtx, _ *core.Node, _ *core.NodePatch, _ *Grant) error {
		return nil // 什么都不授予: 只有 owner 的 "*" 能过
	})
	cms, _ := ctxFor(site)
	cms.SetActor(Actor{NodeID: 1, NodeType: "member", Realm: "frontend", Roles: []string{types.RoleOwner}})
	node := &core.Node{ID: 1, Type: "article"}
	err := cms.authorizeUpdate(node, &core.NodePatch{Fields: core.Fields{"title": "甲", "state": "published"}})
	if err != nil {
		t.Fatalf("owner 该被授予全部字段: %v", err)
	}
	// 非 owner 且规则什么都没给 ⇒ 403（不是 422: 这个动作就没有授权）
	plain := memberCtx(t, site, "editor")
	err = plain.authorizeUpdate(node, &core.NodePatch{Fields: core.Fields{"title": "甲"}})
	var denied *Error
	if !errors.As(err, &denied) || denied.Status != http.StatusForbidden {
		t.Fatalf("err = %v", err)
	}
}

// 注册期: 类型名写错 / 规则为 nil ⇒ panic（拼错的策略不能静默永不生效）。
func TestPolicyRegistrationPanics(t *testing.T) {
	site := newPolicySite(t)
	cases := []struct {
		name string
		run  func()
	}{
		{"类型不存在", func() { site.Type("artikel") }},
		{"OnRead 为 nil", func() { site.Type("article").OnRead(nil) }},
		{"OnCreate 为 nil", func() { site.Type("article").OnCreate(nil) }},
		{"OnUpdate 为 nil", func() { site.Type("article").OnUpdate(nil) }},
		{"OnDelete 为 nil", func() { site.Type("article").OnDelete(nil) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("该 panic")
				}
			}()
			test.run()
		})
	}
}

// 同一个类型多次取句柄 = 同一份策略（链式注册不会互相覆盖）。
func TestTypePolicyHandleIsStable(t *testing.T) {
	site := newPolicySite(t)
	first := site.Type("article")
	second := site.Type("article")
	if first != second {
		t.Fatal("同一类型该拿到同一个句柄")
	}
	first.OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	if site.policy("article").OnRead == nil {
		t.Fatal("注册该落到策略上")
	}
}

func ptrInt64(v int64) *int64 { return &v }
