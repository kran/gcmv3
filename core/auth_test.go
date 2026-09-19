package core

import (
	"errors"
	"strings"
	"testing"

	"github.com/kran/gcmv3/types"
)

// 可登录的类型: roles 词表由能力声明, roles 字段由能力注入。
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

func openAuthFixture(t *testing.T) *GCM {
	t.Helper()
	gcm := openFixture(t)
	ts := types.New()
	err := ts.Load([]byte(authTypesYAML))
	if err != nil {
		t.Fatal(err)
	}
	gcm.types = ts
	gcm.compiler = NewCompiler(ts)
	return gcm
}

// 注册 = 建节点 + 绑凭据, 同一个事务。
func TestRegisterAuth(t *testing.T) {
	gcm := openAuthFixture(t)
	id, err := gcm.RegisterAuth(nil, "member", "email", "a@x.com",
		Fields{"password": "hash"}, &Node{Fields: Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	node, err := gcm.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	if node.Type != "member" || node.Fields.Str("name") != "甲" {
		t.Fatalf("节点 = %#v", node.Fields)
	}
	// roles 字段由能力注入, 默认空
	if roles, ok := node.Fields["roles"].([]any); ok && len(roles) != 0 {
		t.Fatalf("roles 该是空的: %#v", roles)
	}
	method, err := gcm.FindAuth("member", "email", "a@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if method == nil || method.NodeID != id || method.Data.Str("password") != "hash" {
		t.Fatalf("凭据 = %#v", method)
	}
}

// 注册失败（标识已占用）时, 节点也不能留下 —— 两件事必须同事务。
func TestRegisterAuthAtomic(t *testing.T) {
	gcm := openAuthFixture(t)
	_, err := gcm.RegisterAuth(nil, "member", "email", "dup@x.com", Fields{}, &Node{Fields: Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = gcm.RegisterAuth(nil, "member", "email", "dup@x.com", Fields{}, &Node{Fields: Fields{"name": "乙"}})
	if err == nil {
		t.Fatal("同一标识重复注册必须被拒")
	}
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("该归到 ErrDuplicate（上层据此回 409）: %v", err)
	}
	// 关键: 第二次的节点**不该**留下（否则就是个登不进来的僵尸账号）
	if count := nodeCount(t, gcm, "member"); count != 1 {
		t.Fatalf("失败的注册留下了 %d 个节点（该只有 1 个）", count)
	}
}

// 类型必须声明 authentication 能力; 标识必须显式。
func TestRegisterAuthValidation(t *testing.T) {
	gcm := openAuthFixture(t)
	cases := []struct {
		name                         string
		nodeType, method, identifier string
		want                         string
	}{
		{"类型不存在", "ghost", "email", "a@x.com", "not defined"},
		{"类型不能登录", "article", "email", "a@x.com", "not auth-enabled"},
		{"缺标识", "member", "email", "", "method and identifier required"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := gcm.RegisterAuth(nil, test.nodeType, test.method, test.identifier, Fields{}, &Node{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
}

// 一个节点多条凭据; 标识在**类型内**唯一（不同类型各自有命名空间）。
func TestAuthMethodsPerNode(t *testing.T) {
	gcm := openAuthFixture(t)
	id, err := gcm.RegisterAuth(nil, "member", "email", "a@x.com", Fields{"password": "h"}, &Node{Fields: Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	err = gcm.AddAuthMethod(nil, "member", id, "phone", "13800138000", Fields{"code": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := gcm.FindAuth("member", "phone", "13800138000"); m == nil || m.NodeID != id {
		t.Fatalf("phone 凭据 = %#v", m)
	}
	// 同一个标识再绑一次 ⇒ 拒
	if err := gcm.AddAuthMethod(nil, "member", id, "phone", "13800138000", Fields{}); err == nil {
		t.Fatal("重复绑定必须被拒")
	}
	// 节点类型对不上 ⇒ 拒
	if err := gcm.AddAuthMethod(nil, "member", int64(9999), "phone", "x", Fields{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// SetAuthMethod: 不存在则建; 已存在则**覆盖 data**; 属于别的节点 ⇒ 拒。
func TestSetAuthMethod(t *testing.T) {
	gcm := openAuthFixture(t)
	a, err := gcm.CreateNode(nil, &Node{Type: "member", Fields: Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := gcm.CreateNode(nil, &Node{Type: "member", Fields: Fields{"name": "乙"}})
	if err != nil {
		t.Fatal(err)
	}
	// 不存在 ⇒ 建
	if err := gcm.SetAuthMethod(nil, "member", a, "email", "a@x.com", Fields{"password": "h1"}); err != nil {
		t.Fatal(err)
	}
	// 已存在 ⇒ 覆盖 data（改密码）
	if err := gcm.SetAuthMethod(nil, "member", a, "email", "a@x.com", Fields{"password": "h2"}); err != nil {
		t.Fatal(err)
	}
	m, _ := gcm.FindAuth("member", "email", "a@x.com")
	if m == nil || m.Data.Str("password") != "h2" {
		t.Fatalf("改密码没生效: %#v", m)
	}
	// 抢别人的标识 ⇒ 拒
	err = gcm.SetAuthMethod(nil, "member", b, "email", "a@x.com", Fields{"password": "h3"})
	if err == nil || !strings.Contains(err.Error(), "another node") {
		t.Fatalf("err = %v", err)
	}
}

// 不许把最后一个凭据删掉。
func TestRemoveAuthMethodKeepsLast(t *testing.T) {
	gcm := openAuthFixture(t)
	id, err := gcm.RegisterAuth(nil, "member", "email", "a@x.com", Fields{}, &Node{Fields: Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	err = gcm.RemoveAuthMethod(nil, "member", "email", "a@x.com")
	if err == nil || !strings.Contains(err.Error(), "cannot remove last") {
		t.Fatalf("err = %v", err)
	}
	// 加一条之后就能删了
	err = gcm.AddAuthMethod(nil, "member", id, "phone", "138", Fields{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gcm.RemoveAuthMethod(nil, "member", "email", "a@x.com"); err != nil {
		t.Fatal(err)
	}
	if m, _ := gcm.FindAuth("member", "email", "a@x.com"); m != nil {
		t.Fatalf("删完还在: %#v", m)
	}
}

// 会话: 建 / 解 / 注销。库里只留哈希（令牌本身不落库）。
func TestSessions(t *testing.T) {
	gcm := openAuthFixture(t)
	id, err := gcm.RegisterAuth(nil, "member", "email", "a@x.com", Fields{}, &Node{Fields: Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := gcm.CreateSession(nil, "frontend", id)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("令牌为空")
	}
	// 库里只有哈希
	var stored string
	row, err := gcm.db.Add(`SELECT token_hash FROM sessions`).FetchOne[string]()
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		stored = *row
	}
	if stored == token || stored != sessionTokenHash(token) {
		t.Fatalf("库里该只有哈希: %q", stored)
	}
	// 解令牌
	session, err := gcm.ValidSession(token)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil || session.NodeID != id || session.Realm != "frontend" {
		t.Fatalf("会话 = %#v", session)
	}
	// 假令牌 / 空令牌 → nil, 不报错
	if s, _ := gcm.ValidSession("bogus"); s != nil {
		t.Fatalf("假令牌该返回 nil: %#v", s)
	}
	if s, _ := gcm.ValidSession(""); s != nil {
		t.Fatalf("空令牌该返回 nil: %#v", s)
	}
	// 登出
	if err := gcm.DeleteSession(nil, token); err != nil {
		t.Fatal(err)
	}
	if s, _ := gcm.ValidSession(token); s != nil {
		t.Fatalf("登出后还能解: %#v", s)
	}
	// 空令牌登出是幂等 no-op
	if err := gcm.DeleteSession(nil, ""); err != nil {
		t.Fatal(err)
	}
}

// 过期会话解不出来（纯读, 不清库）。
func TestSessionExpiry(t *testing.T) {
	gcm := openAuthFixture(t)
	id, err := gcm.RegisterAuth(nil, "member", "email", "a@x.com", Fields{}, &Node{Fields: Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := gcm.CreateSession(nil, "frontend", id)
	if err != nil {
		t.Fatal(err)
	}
	// 手工把过期时间推到过去
	_, err = gcm.db.Update("sessions", map[string]any{"expires_at": nowValue() - 1},
		`token_hash = #{1}`, sessionTokenHash(token)).Exec()
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := gcm.ValidSession(token); s != nil {
		t.Fatalf("过期会话该解不出来: %#v", s)
	}
}

// 会话要绑在**可登录**的节点上; realm 必填。
func TestSessionValidation(t *testing.T) {
	gcm := openAuthFixture(t)
	article, err := gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{"title": "文"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gcm.CreateSession(nil, "frontend", article); err == nil {
		t.Fatal("非可登录类型不该能开会话")
	}
	if _, err := gcm.CreateSession(nil, "", article); err == nil {
		t.Fatal("realm 必填")
	}
	if _, err := gcm.CreateSession(nil, "frontend", int64(9999)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// 删节点: 凭据与会话靠外键级联清掉（foreign_keys=1 是连接档位的一部分）。
func TestAuthCascadeOnNodeDelete(t *testing.T) {
	gcm := openAuthFixture(t)
	id, err := gcm.RegisterAuth(nil, "member", "email", "a@x.com", Fields{}, &Node{Fields: Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gcm.CreateSession(nil, "frontend", id); err != nil {
		t.Fatal(err)
	}
	if err := gcm.DeleteNode(nil, id); err != nil {
		t.Fatal(err)
	}
	if m, _ := gcm.FindAuth("member", "email", "a@x.com"); m != nil {
		t.Fatalf("节点删了凭据还在: %#v", m)
	}
	hashes, err := gcm.db.Add(`SELECT token_hash FROM sessions`).FetchList[string]()
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 0 {
		t.Fatalf("节点删了会话还在: %#v", hashes)
	}
}

// 踢人: 删掉某节点的全部会话（其它节点不受影响）。
func TestDeleteNodeSessions(t *testing.T) {
	gcm := openAuthFixture(t)
	a, err := gcm.RegisterAuth(nil, "member", "email", "a@x.com", Fields{}, &Node{Fields: Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := gcm.RegisterAuth(nil, "member", "email", "b@x.com", Fields{}, &Node{Fields: Fields{"name": "乙"}})
	if err != nil {
		t.Fatal(err)
	}
	tokenA, _ := gcm.CreateSession(nil, "frontend", a)
	tokenB, _ := gcm.CreateSession(nil, "frontend", b)
	if err := gcm.DeleteNodeSessions(nil, a); err != nil {
		t.Fatal(err)
	}
	if s, _ := gcm.ValidSession(tokenA); s != nil {
		t.Fatalf("甲的会话该没了: %#v", s)
	}
	if s, _ := gcm.ValidSession(tokenB); s == nil {
		t.Fatal("乙的会话不该受影响")
	}
	// 两次令牌互不相同（随机）
	if tokenA == tokenB {
		t.Fatal("令牌不该重复")
	}
}
