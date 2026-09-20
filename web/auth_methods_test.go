package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	so "github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// 每个声明了 authentication 的类型自动有一条**同名渠道**（否则"类型声明了能登录"
// 却登不进来 —— 每个站点都要记得写一遍 Register, 漏了就是后台登录不了）。
func TestAuthRealmAutoRegistered(t *testing.T) {
	site := newPolicySite(t)
	names := map[string]bool{}
	for _, realm := range site.Auth().Realms() {
		names[realm.Name] = true
		// 自动那条名字 = 类型名（夹具自己另外显式注册了 "frontend", 不在此列）
		if realm.Name == realm.NodeType {
			continue
		}
		if _, isAuthType := site.types.AuthMethods(realm.Name); isAuthType {
			t.Fatalf("自动渠道该与类型同名: %#v", realm)
		}
	}
	for _, want := range []string{"member", "staff"} {
		if !names[want] {
			t.Fatalf("类型 %q 声明了 authentication 却没有渠道: %#v", want, site.Auth().Realms())
		}
	}

	// 站点显式 Register **覆盖**自动那条（补 RegisterMethods / Default）
	site.Auth().Register(AuthRealm{Name: "member", NodeType: "member",
		RegisterMethods: []string{"email"}})
	realm, ok := site.Auth().realm("member")
	if !ok || len(realm.RegisterMethods) != 1 {
		t.Fatalf("显式注册该覆盖自动那条: %#v", realm)
	}
}

// RegisterMethods 必须 ⊆ 类型声明的 methods（否则注册出来的凭据没人核验）。
func TestRegisterMethodsMustBeDeclared(t *testing.T) {
	site := newPolicySite(t)
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("注册一个未声明的方式该 panic")
		}
		if !strings.Contains(recovered.(string), "authentication.methods") {
			t.Fatalf("错误信息要点名 authentication.methods: %v", recovered)
		}
	}()
	site.Auth().Register(AuthRealm{Name: "member", NodeType: "member", RegisterMethods: []string{"wechat"}})
}

// **后门测试**: 别的登录机制（微信那种）的凭据里一旦有 password, 拿它当口令登录
// 必须被拒 —— 那条凭据该只走插件自己的核验。
func TestPasswordLoginRejectsUndeclaredMethod(t *testing.T) {
	site := newPolicySite(t)
	// 登录成功后框架会按读规则回读节点（fail-loud: 忘了注册读规则就 403）
	site.Type("member").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	cms := memberCtx(t, site)
	methods, ok := site.types.AuthMethods("member")
	if !ok || len(methods) == 0 {
		t.Fatal("member 该有默认口令方式")
	}

	// 造一条 method=wechat 的凭据, data 里塞 password（模拟"被谁写进了哈希"）
	nodeID, err := site.Engine().CreateNode(nil, &core.Node{
		Type: "member", Fields: core.Fields{"name": "甲"},
	})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hashPassword("secret123")
	if err != nil {
		t.Fatal(err)
	}
	err = site.Engine().AddAuthMethod(nil, "member", nodeID, "wechat", "openid_abc",
		core.Fields{passwordKey: hash})
	if err != nil {
		t.Fatal(err)
	}
	_ = cms

	response := jsonDo(t, site, http.MethodPost, "/api/auth/member/login",
		`{"method":"wechat","identifier":"openid_abc","secret":"secret123"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("未声明的方式不该能走口令登录, 实际 = %d: %s", response.Code, response.Body.String())
	}

	// 声明的口令方式照常能登（证明确实是"方式"被拒, 不是别的地方坏了）
	err = site.Engine().AddAuthMethod(nil, "member", nodeID, "email", "b@x.com",
		core.Fields{passwordKey: hash})
	if err != nil {
		t.Fatal(err)
	}
	response = jsonDo(t, site, http.MethodPost, "/api/auth/member/login",
		`{"method":"email","identifier":"b@x.com","secret":"secret123"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("声明的方式该能登录, 实际 = %d: %s", response.Code, response.Body.String())
	}
}

// roles 只有 owner 能写 —— **框架不变量**（站点规则里不用再写一遍）。
//
// 这里特意让更新规则把 "*" 授给所有人: 不变量放在校验点（而不是从 grant 里删字段），
// "*" 也绕不过去。测的是 member（有 authentication 能力 ⇒ 才有 roles 字段）。
func TestRolesOnlyOwnerFrameworkInvariant(t *testing.T) {
	site := newPolicySite(t)
	site.Type("member").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	site.Type("member").OnUpdate(func(_ *CmsCtx, _ *core.Node, _ *core.NodePatch, allow *Grant) error {
		allow.Add("public", grantAll)
		return nil
	})

	// 非 owner: 提交 roles ⇒ 被拒（即使规则给了 "*"）
	plain, selfID := newMember(t, site, "0")
	_, err := plain.Update("member", selfID, &core.NodePatch{
		Revision: ptrInt64(1), Fields: core.Fields{types.RolesField: []any{types.RoleOwner}},
	})
	if err == nil {
		t.Fatal("非 owner 写 roles 该被拒")
	}
	var appErr *Error
	if !errors.As(err, &appErr) {
		t.Fatalf("该是字段级错误: %v", err)
	}
	if reason := appErr.Details[types.RolesField]; !strings.Contains(reason, "只有 owner") {
		t.Fatalf("roles 的拒绝原因该点明只有 owner: %#v", appErr.Details)
	}

	// owner: 可以写
	owner, ownerID := newMember(t, site, "1", types.RoleOwner)
	_, err = owner.Update("member", ownerID, &core.NodePatch{
		Revision: ptrInt64(1), Fields: core.Fields{types.RolesField: []any{types.RoleAdmin}},
	})
	if err != nil {
		t.Fatalf("owner 该能写 roles: %v", err)
	}
}

// newMember 造一个会员身份 + 会话（每次邮箱不同 —— 表上 (type, method, identifier) 唯一）。
func newMember(t *testing.T, site *Site, suffix string, roles ...string) (*CmsCtx, int64) {
	t.Helper()
	values := make([]any, 0, len(roles))
	for _, role := range roles {
		values = append(values, role)
	}
	id, err := site.Engine().RegisterAuth(nil, "member", "email", "m"+suffix+"@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "会员" + suffix, "roles": values}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "frontend", id)
	if err != nil {
		t.Fatal(err)
	}
	cms, _ := ctxFor(site, withBearer(token))
	return cms, id
}

// 事实与校验同源: 非 owner 的可写集合里不能出现 roles（界面不能显示"可编辑"然后被拒）。
func TestRolesExcludedFromEditableForNonOwner(t *testing.T) {
	site := newPolicySite(t)
	allowArticleCreate(site)
	site.Type("article").OnCreate(func(_ *CmsCtx, _ *core.Node, allow *Grant) error {
		allow.Add("public", grantAll)
		return nil
	})
	site.Type("article").OnUpdate(func(_ *CmsCtx, _ *core.Node, _ *core.NodePatch, allow *Grant) error {
		allow.Add("public", grantAll)
		return nil
	})
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})

	member := memberCtx(t, site)
	created, err := member.Create("article", core.Fields{"title": "甲"})
	if err != nil {
		t.Fatal(err)
	}
	one, err := member.Get("article", created.ID)
	if err != nil || one == nil {
		t.Fatalf("get: %v %v", one, err)
	}
	for _, name := range one.Editable {
		if name == types.RolesField {
			t.Fatalf("非 owner 的可写集合不该含 roles: %#v", one.Editable)
		}
	}
}
