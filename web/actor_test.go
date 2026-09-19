package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/types"
)

// 可登录的类型 + 词表（types 会注入 roles 字段）。
const authTypesYAML = `
types:
  member:
    capabilities:
      authentication: { roles: [editor] }
    fields:
      - { name: name, kind: text }
  article:
    fields:
      - { name: title, kind: text }
`

func newAuthSite(t *testing.T) *Site {
	t.Helper()
	basedir := t.TempDir()
	err := writeTypesFile(basedir, authTypesYAML)
	if err != nil {
		t.Fatal(err)
	}
	site, err := Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = site.Close() })
	return site
}

// ctxFor 造一个请求上下文（可带 cookie / bearer）。
func ctxFor(site *Site, opts ...func(*http.Request)) (*CmsCtx, *httptest.ResponseRecorder) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, opt := range opts {
		opt(request)
	}
	recorder := httptest.NewRecorder()
	return site.CmsCtxMaker(recorder, request), recorder
}

func withCookie(name, value string) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: name, Value: value}) }
}

func withBearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

// 没有凭据 ⇒ 匿名（NodeID 0, 没有角色）。
func TestActorAnonymous(t *testing.T) {
	site := newAuthSite(t)
	cms, _ := ctxFor(site)
	actor := cms.Actor()
	if !actor.IsAnonymous() || actor.NodeID != 0 {
		t.Fatalf("该是匿名: %#v", actor)
	}
	if len(actor.Roles) != 0 {
		t.Fatalf("匿名没有角色: %#v", actor.Roles)
	}
	if _, err := cms.Principal(); !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("err = %v", err)
	}
}

// 双轨: cookie 与 Bearer 等价。
func TestActorBothCarriers(t *testing.T) {
	site := newAuthSite(t)
	id, err := site.Engine().RegisterAuth(nil, "member", "email", "a@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "甲", "roles": []any{"editor"}}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "frontend", id)
	if err != nil {
		t.Fatal(err)
	}

	for _, carrier := range []struct {
		name string
		opt  func(*http.Request)
	}{
		{"bearer", withBearer(token)},
		{"cookie", withCookie(TokenCookie, token)},
	} {
		t.Run(carrier.name, func(t *testing.T) {
			cms, _ := ctxFor(site, carrier.opt)
			actor := cms.Actor()
			if actor.NodeID != id || actor.NodeType != "member" || actor.Realm != "frontend" {
				t.Fatalf("身份 = %#v", actor)
			}
			// 角色 = 类型名 + 词表字段
			if !actor.HasRole("member") || !actor.HasRole("editor") {
				t.Fatalf("角色 = %#v", actor.Roles)
			}
			if actor.IsAnonymous() {
				t.Fatal("不该是匿名")
			}
			// Principal 拿到的是业务节点（Fields 完整）
			node, err := cms.Principal()
			if err != nil {
				t.Fatal(err)
			}
			if node.ID != id || node.Fields.Str("name") != "甲" {
				t.Fatalf("principal = %#v", node)
			}
		})
	}
}

// 令牌错 / 过期 / 节点没了 ⇒ 匿名（不是报错, 也不是 500）。
func TestActorInvalidTokenFallsBackToAnonymous(t *testing.T) {
	site := newAuthSite(t)
	id, err := site.Engine().RegisterAuth(nil, "member", "email", "a@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "frontend", id)
	if err != nil {
		t.Fatal(err)
	}

	// 假令牌
	cms, _ := ctxFor(site, withBearer("bogus"))
	if !cms.Actor().IsAnonymous() {
		t.Fatalf("假令牌该匿名: %#v", cms.Actor())
	}
	// 过期（手工把 expires_at 推到过去）
	_, err = site.DB().Update("sessions", map[string]any{"expires_at": int64(1)},
		`realm = #{1}`, "frontend").Exec()
	if err != nil {
		t.Fatal(err)
	}
	cms, _ = ctxFor(site, withBearer(token))
	if !cms.Actor().IsAnonymous() {
		t.Fatalf("过期令牌该匿名: %#v", cms.Actor())
	}
	// 节点被删（会话靠外键级联清掉 ⇒ 也是匿名）
	token, err = site.Engine().CreateSession(nil, "frontend", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := site.Engine().DeleteNode(nil, id); err != nil {
		t.Fatal(err)
	}
	cms, _ = ctxFor(site, withBearer(token))
	if !cms.Actor().IsAnonymous() {
		t.Fatalf("节点没了该匿名: %#v", cms.Actor())
	}
}

// 别的 scheme（Basic）不认; 裸令牌认（有些客户端直接塞 header）。
func TestActorCarrierForms(t *testing.T) {
	site := newAuthSite(t)
	cms, _ := ctxFor(site, func(r *http.Request) {
		r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	})
	if !cms.Actor().IsAnonymous() {
		t.Fatalf("Basic 不该被当令牌: %#v", cms.Actor())
	}
	cms, _ = ctxFor(site, func(r *http.Request) {
		r.Header.Set("Authorization", "raw-token")
	})
	// 认是认（当令牌去解）, 但它是假令牌 ⇒ 匿名; 关键是别 panic、别当成 Bearer 前缀问题
	if !cms.Actor().IsAnonymous() {
		t.Fatalf("假裸令牌该匿名: %#v", cms.Actor())
	}
}

// 懒解析 + 缓存: 一次请求内只解析一次（第二次读缓存, 不再查库）。
//
// 验法用行为而不是查库次数: 解析完之后把会话删掉 —— 若第二次还去查, 就会退回匿名。
func TestActorResolvedOnce(t *testing.T) {
	site := newAuthSite(t)
	id, err := site.Engine().RegisterAuth(nil, "member", "email", "a@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "frontend", id)
	if err != nil {
		t.Fatal(err)
	}
	cms, _ := ctxFor(site, withBearer(token))
	if cms.Actor().NodeID != id {
		t.Fatalf("第一次该解出身份: %#v", cms.Actor())
	}
	// 把会话删掉（缓存之外的真相变了）
	_, err = site.DB().Delete("sessions", `node_id = #{1}`, id).Exec()
	if err != nil {
		t.Fatal(err)
	}
	if cms.Actor().NodeID != id {
		t.Fatal("同一请求内第二次该走缓存（不再查库）")
	}
	// 新请求则按库里的真相来 ⇒ 撤销即时生效
	fresh, _ := ctxFor(site, withBearer(token))
	if !fresh.Actor().IsAnonymous() {
		t.Fatalf("新请求该看到撤销: %#v", fresh.Actor())
	}
}

// SetActor: 插件登录成功那一个请求内直接装身份; 匿名装回去也认。
func TestSetActor(t *testing.T) {
	site := newAuthSite(t)
	cms, _ := ctxFor(site)
	cms.SetActor(Actor{NodeID: 7, NodeType: "member", Realm: "frontend", Roles: []string{"member"}})
	actor := cms.Actor()
	if actor.NodeID != 7 || !actor.HasRole("member") {
		t.Fatalf("装上的身份 = %#v", actor)
	}
	cms.SetActor(Actor{})
	if !cms.Actor().IsAnonymous() {
		t.Fatalf("该退回匿名: %#v", cms.Actor())
	}
	// 不完整的节点身份要响亮地炸（fail-loud, 不是悄悄当匿名）
	defer func() {
		if recover() == nil {
			t.Fatal("不完整的 node actor 该 panic")
		}
	}()
	cms.SetActor(Actor{NodeID: 7})
}

// 角色取值容错: roles 字段可能是 []any（JSON）或 []string。
func TestRolesOfForms(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  []string
	}{
		{"json 数组", []any{"owner", "editor"}, []string{"member", "owner", "editor"}},
		{"字符串切片", []string{"admin"}, []string{"member", "admin"}},
		{"混入非字符串", []any{"owner", 7, ""}, []string{"member", "owner"}},
		{"缺失", nil, []string{"member"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			node := &core.Node{Type: "member", Fields: core.Fields{}}
			if test.value != nil {
				node.Fields[types.RolesField] = test.value
			}
			got := rolesOf(node)
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("roles = %#v, want %#v", got, test.want)
			}
		})
	}
}
