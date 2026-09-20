package web

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	so "github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// 凭据管理**只有 owner** —— admin 角色能进后台, 但改别人密码 = 能冒充别人。
func TestAdminAuthRequiresOwner(t *testing.T) {
	site := newPolicySiteWithMemberRead(t)
	admin, targetID := newMember(t, site, "adm", types.RoleAdmin)

	response := jsonDo(t, site, http.MethodGet,
		"/admin/auth/member/"+strconv.FormatInt(targetID, 10), "", bearerOf(t, site, admin))
	if response.Code != http.StatusForbidden {
		t.Fatalf("admin 角色不该能读凭据, 实际 %d: %s", response.Code, response.Body.String())
	}
	response = jsonDo(t, site, http.MethodPost,
		"/admin/auth/member/"+strconv.FormatInt(targetID, 10), `{"method":"email","identifier":"x@y.com","secret":"secret123"}`,
		bearerOf(t, site, admin))
	if response.Code != http.StatusForbidden {
		t.Fatalf("admin 角色不该能设凭据, 实际 %d: %s", response.Code, response.Body.String())
	}
}

// owner 设了口令 ⇒ 那个节点就能用它登录; 改完旧会话被踢掉。
func TestAdminAuthSetThenLogin(t *testing.T) {
	site := newPolicySiteWithMemberRead(t)
	owner := ownerCtx(t, site)
	targetID := createMemberNode(t, site, "被改的人")

	// 目标节点先有一条会话（模拟"已经登录着"）
	token, err := site.Engine().CreateSession(nil, "frontend", targetID)
	if err != nil {
		t.Fatal(err)
	}
	if session, _ := site.Engine().ValidSession(token); session == nil {
		t.Fatal("前提: 会话该有效")
	}

	response := jsonDo(t, site, http.MethodPost, "/admin/auth/member/"+strconv.FormatInt(targetID, 10),
		`{"method":"email","identifier":"new@x.com","secret":"secret123"}`, bearerOf(t, site, owner))
	if response.Code != http.StatusNoContent {
		t.Fatalf("owner 该能设口令, 实际 %d: %s", response.Code, response.Body.String())
	}
	// 踢会话: 改密码就该把旧登录态作废（否则"把坏人踢下线"是空话）
	if session, _ := site.Engine().ValidSession(token); session != nil {
		t.Fatal("改凭据后旧会话该失效")
	}
	// 新口令能登录
	login := jsonDo(t, site, http.MethodPost, "/api/auth/member/login",
		`{"method":"email","identifier":"new@x.com","secret":"secret123"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("新口令该能登录, 实际 %d: %s", login.Code, login.Body.String())
	}

	// 列表: 只有方式/标识/时间（哈希永不出库）
	list := jsonDo(t, site, http.MethodGet, "/admin/auth/member/"+strconv.FormatInt(targetID, 10), "",
		bearerOf(t, site, owner))
	if list.Code != http.StatusOK {
		t.Fatalf("列表 = %d: %s", list.Code, list.Body.String())
	}
	body := list.Body.String()
	if !strings.Contains(body, "new@x.com") {
		t.Fatalf("列表该有刚设的标识: %s", body)
	}
	// 只查真正的凭据特征（bcrypt 前缀 / data 字段）—— 响应里出现 "password_methods"
	// 这个键名是设计如此（它列的是"哪些方式能设口令", 不是凭据）
	if strings.Contains(body, "$2a$") || strings.Contains(body, `"data"`) {
		t.Fatalf("列表里不该出现凭据哈希: %s", body)
	}

	// 先加第二条凭据: 解绑"最后一条"是被规则挡住的（另有测试）
	second := jsonDo(t, site, http.MethodPost, "/admin/auth/member/"+strconv.FormatInt(targetID, 10),
		`{"method":"phone","identifier":"13800138000","secret":"secret123"}`, bearerOf(t, site, owner))
	if second.Code != http.StatusNoContent {
		t.Fatalf("第二条凭据 = %d: %s", second.Code, second.Body.String())
	}

	// 解绑 ⇒ 登不上了
	remove := jsonDo(t, site, http.MethodDelete, "/admin/auth/member/"+strconv.FormatInt(targetID, 10)+"/email", "",
		bearerOf(t, site, owner))
	if remove.Code != http.StatusNoContent {
		t.Fatalf("解绑 = %d: %s", remove.Code, remove.Body.String())
	}
	login = jsonDo(t, site, http.MethodPost, "/api/auth/member/login",
		`{"method":"email","identifier":"new@x.com","secret":"secret123"}`)
	if login.Code == http.StatusOK {
		t.Fatal("解绑之后不该还能登录")
	}
}

// 删掉最后一条凭据被规则挡住 ⇒ **400 + 人话**, 不是 500 服务端错误。
func TestAdminAuthRemoveLastIsNotServerError(t *testing.T) {
	site := newPolicySiteWithMemberRead(t)
	owner := ownerCtx(t, site)
	targetID := createMemberNode(t, site, "只有一条凭据的人")
	_ = jsonDo(t, site, http.MethodPost, "/admin/auth/member/"+strconv.FormatInt(targetID, 10),
		`{"method":"email","identifier":"only@x.com","secret":"secret123"}`, bearerOf(t, site, owner))

	response := jsonDo(t, site, http.MethodDelete,
		"/admin/auth/member/"+strconv.FormatInt(targetID, 10)+"/email", "", bearerOf(t, site, owner))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("删最后一条该 400（不是 500）, 实际 %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "最后一条") {
		t.Fatalf("错误信息该说人话: %s", response.Body.String())
	}
}

// 不能给"框架不管的方式"设口令（否则就是把一条本该只认 openid 的凭据写成口令）。
func TestAdminAuthRejectsUndeclaredMethod(t *testing.T) {
	site := newPolicySiteWithMemberRead(t)
	owner := ownerCtx(t, site)
	targetID := createMemberNode(t, site, "甲")

	response := jsonDo(t, site, http.MethodPost, "/admin/auth/member/"+strconv.FormatInt(targetID, 10),
		`{"method":"wechat","identifier":"openid_abc","secret":"secret123"}`, bearerOf(t, site, owner))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("未声明的方式该被拒, 实际 %d: %s", response.Code, response.Body.String())
	}

	// 不是 auth 能力的类型 / 不存在的节点 ⇒ 也 fail-loud
	response = jsonDo(t, site, http.MethodGet, "/admin/auth/article/"+strconv.FormatInt(targetID, 10), "",
		bearerOf(t, site, owner))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("非 auth 类型该 400, 实际 %d: %s", response.Code, response.Body.String())
	}
	response = jsonDo(t, site, http.MethodGet, "/admin/auth/member/999999", "",
		bearerOf(t, site, owner))
	if response.Code != http.StatusNotFound {
		t.Fatalf("不存在的节点该 404, 实际 %d: %s", response.Code, response.Body.String())
	}
}

// newPolicySiteWithMemberRead 读规则: member 全放行（登录/回读都要它）。
func newPolicySiteWithMemberRead(t *testing.T) *Site {
	t.Helper()
	site := newPolicySite(t)
	site.Type("member").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	return site
}

// ownerCtx 一个 owner 会员身份（够后台的 owner||admin 门, 也够凭据管理的 owner 检查）。
func ownerCtx(t *testing.T, site *Site) *CmsCtx {
	t.Helper()
	cms, _ := newMember(t, site, "owner", types.RoleOwner)
	return cms
}

// createMemberNode 直接建一个会员节点（不经策略 —— 测试夹具）。
func createMemberNode(t *testing.T, site *Site, name string) int64 {
	t.Helper()
	id, err := site.Engine().CreateNode(nil, &core.Node{Type: "member", Fields: core.Fields{"name": name}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// bearerOf 这个 ctx 的令牌（测试里要拿它发后续请求）。
func bearerOf(t *testing.T, site *Site, cms *CmsCtx) func(*http.Request) {
	t.Helper()
	actor := cms.Actor()
	token, err := site.Engine().CreateSession(nil, actor.Realm, actor.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	return withBearer(token)
}
