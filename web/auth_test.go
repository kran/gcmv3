package web

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
)

// realmTypesYAML 两个渠道: 会员（可注册）与员工（不可注册, 后台走它）。
const realmTypesYAML = `
types:
  member:
    capabilities:
      authentication: { roles: [vip] }
    fields:
      - { name: name, kind: text }
      - { name: phone, kind: text }
  staff:
    capabilities:
      authentication: { roles: [editor] }
    fields:
      - { name: name, kind: text }
`

// authSite 会员可自助注册（email）, 员工不可注册（后台渠道）。
func authSite(t *testing.T) *Site {
	t.Helper()
	basedir := t.TempDir()
	err := writeTypesFile(basedir, realmTypesYAML)
	if err != nil {
		t.Fatal(err)
	}
	site, err := Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = site.Close() })
	site.Auth().Register(AuthRealm{Name: "member", NodeType: "member",
		RegisterMethods: []string{"email"}, Default: true})
	site.Auth().Register(AuthRealm{Name: "staff", NodeType: "staff"})
	// 读规则: 两种类型都公开可读（登录响应要掩码 user ⇒ 需要规则）
	openAll := func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	}
	site.Type("member").OnRead(openAll)
	site.Type("staff").OnRead(openAll)
	// 注册钩子: **白名单拷贝**客户端字段 + 补默认值（这就是该有的写法）
	site.Hook(HookAuthRegister, func(_ *CmsCtx, realm AuthRealm, input *RegisterInput, node *core.Node) error {
		if realm.Name != "member" {
			return Forbidden("该渠道不允许注册")
		}
		for _, name := range []string{"name", "phone"} {
			value, ok := input.Fields[name]
			if ok && value != nil {
				node.Fields[name] = value
			}
		}
		return nil
	})
	return site
}

// 注册 → 建节点 + 建凭据（同一事务）+ 直接给会话。
func TestAuthRegisterAndLogin(t *testing.T) {
	site := authSite(t)
	got := jsonDo(t, site, http.MethodPost, "/api/auth/member/register",
		`{"method":"email","identifier":"a@x.com","secret":"secret123","fields":{"name":"甲"}}`)
	if got.Code != http.StatusOK {
		t.Fatalf("注册 = %d %q", got.Code, got.Body.String())
	}
	body := got.Body.String()
	if !strings.Contains(body, `"token"`) || !strings.Contains(body, `"甲"`) {
		t.Fatalf("注册响应该带令牌与用户: %q", body)
	}
	if !strings.Contains(body, `"realm":"member"`) {
		t.Fatalf("身份该带渠道: %q", body)
	}
	// 库里: 口令是 bcrypt 哈希, 不是明文
	method, err := site.Engine().FindAuth("member", "email", "a@x.com")
	if err != nil || method == nil {
		t.Fatalf("凭据没建上: %v", err)
	}
	hash := method.Data.Str("password")
	if hash == "secret123" || !strings.HasPrefix(hash, "$2") {
		t.Fatalf("该存 bcrypt 哈希: %q", hash)
	}
	// 重复注册同一标识 ⇒ 冲突（唯一索引）
	again := jsonDo(t, site, http.MethodPost, "/api/auth/member/register",
		`{"method":"email","identifier":"a@x.com","secret":"secret123"}`)
	if again.Code != http.StatusConflict {
		t.Fatalf("重复注册 = %d %q", again.Code, again.Body.String())
	}

	// 登录（不带 cookie/bearer, 靠 body 里的凭据）
	login := jsonDo(t, site, http.MethodPost, "/api/auth/member/login",
		`{"method":"email","identifier":"a@x.com","secret":"secret123"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("登录 = %d %q", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != TokenCookie {
		t.Fatalf("该下发 cookie: %#v", cookies)
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie 属性: %#v", cookies[0])
	}
	token := sessionToken(t, login.Body.String())
	// 用 cookie 读 /me
	me := jsonDo(t, site, http.MethodGet, "/api/auth/me", "", withCookie(TokenCookie, token))
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), `"甲"`) {
		t.Fatalf("me = %d %q", me.Code, me.Body.String())
	}
	// 登出 ⇒ 会话没了 + cookie 被清
	out := jsonDo(t, site, http.MethodPost, "/api/auth/logout", "", withCookie(TokenCookie, token))
	if out.Code != http.StatusNoContent {
		t.Fatalf("登出 = %d", out.Code)
	}
	after := jsonDo(t, site, http.MethodGet, "/api/auth/me", "", withCookie(TokenCookie, token))
	if after.Code != http.StatusUnauthorized {
		t.Fatalf("登出后 = %d %q", after.Code, after.Body.String())
	}
}

// 登录失败**统一 401**（不区分没账号/口令错/方式没声明）。
func TestAuthLoginFailures(t *testing.T) {
	site := authSite(t)
	register := jsonDo(t, site, http.MethodPost, "/api/auth/member/register",
		`{"method":"email","identifier":"a@x.com","secret":"secret123"}`)
	if register.Code != http.StatusOK {
		t.Fatalf("注册 = %d", register.Code)
	}
	cases := map[string]string{
		"口令错":     `{"method":"email","identifier":"a@x.com","secret":"wrong123"}`,
		"没这个账号":   `{"method":"email","identifier":"nope@x.com","secret":"secret123"}`,
		"方式没这条凭据": `{"method":"phone","identifier":"a@x.com","secret":"secret123"}`,
	}
	for name, body := range cases {
		got := jsonDo(t, site, http.MethodPost, "/api/auth/member/login", body)
		if got.Code != http.StatusUnauthorized {
			t.Fatalf("%s = %d %q", name, got.Code, got.Body.String())
		}
		// 三种失败的消息必须一样 —— 否则登录端点成了账号枚举器
		if !strings.Contains(got.Body.String(), "账号或口令不正确") {
			t.Fatalf("%s 的消息泄漏了原因: %q", name, got.Body.String())
		}
	}
}

// 渠道不存在 / 不开放注册 / 没声明的方式 ⇒ 各自的拒绝。
func TestAuthRegisterGating(t *testing.T) {
	site := authSite(t)
	// 渠道不存在
	got := jsonDo(t, site, http.MethodPost, "/api/auth/nope/login",
		`{"method":"email","identifier":"a@x.com","secret":"secret123"}`)
	if got.Code != http.StatusNotFound {
		t.Fatalf("渠道不存在 = %d", got.Code)
	}
	// staff 渠道不开放注册
	got = jsonDo(t, site, http.MethodPost, "/api/auth/staff/register",
		`{"method":"username","identifier":"boss","secret":"secret123"}`)
	if got.Code != http.StatusForbidden {
		t.Fatalf("不开放注册 = %d %q", got.Code, got.Body.String())
	}
	// member 只允许 email 注册
	got = jsonDo(t, site, http.MethodPost, "/api/auth/member/register",
		`{"method":"phone","identifier":"138","secret":"secret123"}`)
	if got.Code != http.StatusForbidden {
		t.Fatalf("未声明的方式 = %d %q", got.Code, got.Body.String())
	}
	// 口令太短
	got = jsonDo(t, site, http.MethodPost, "/api/auth/member/register",
		`{"method":"email","identifier":"b@x.com","secret":"short"}`)
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("口令太短 = %d %q", got.Code, got.Body.String())
	}
}

// **提权测试**: 客户端在注册体里塞 roles 不会落进节点。
func TestAuthRegisterCannotSelfEscalate(t *testing.T) {
	site := authSite(t)
	got := jsonDo(t, site, http.MethodPost, "/api/auth/member/register",
		`{"method":"email","identifier":"evil@x.com","secret":"secret123",
		  "fields":{"name":"攻击者","roles":["owner","admin"]}}`)
	if got.Code != http.StatusOK {
		t.Fatalf("注册 = %d %q", got.Code, got.Body.String())
	}
	token := sessionToken(t, got.Body.String())
	// 用它的会话看自己的角色（/me 里有 actor.roles）
	me := jsonDo(t, site, http.MethodGet, "/api/auth/me", "", withBearer(token))
	if me.Code != http.StatusOK {
		t.Fatalf("me = %d", me.Code)
	}
	if strings.Contains(me.Body.String(), "owner") || strings.Contains(me.Body.String(), "admin") {
		t.Fatalf("客户端字段不该落库(提权): %q", me.Body.String())
	}
	// actor 的角色只该有基础角色（public + 类型名）; 客户端塞的 owner/admin 不该出现
	var envelope struct {
		Actor Actor                           `json:"actor"`
		User  struct{ Fields map[string]any } `json:"user"`
	}
	err := json.Unmarshal([]byte(me.Body.String()), &envelope)
	if err != nil {
		t.Fatal(err)
	}
	if len(envelope.Actor.Roles) != 2 || envelope.Actor.Roles[0] != "public" || envelope.Actor.Roles[1] != "member" {
		t.Fatalf("actor 角色 = %#v", envelope.Actor.Roles)
	}
	if _, ok := envelope.User.Fields["roles"]; ok {
		t.Fatalf("user.fields 不该有 roles: %#v", envelope.User.Fields)
	}
	// 库里也确认一遍
	method, err := site.Engine().FindAuth("member", "email", "evil@x.com")
	if err != nil || method == nil {
		t.Fatalf("%v", err)
	}
	node, err := site.Engine().GetNode(method.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := node.Fields["roles"]; ok {
		t.Fatalf("roles 不该被写入: %#v", node.Fields)
	}
	// 钩子拷贝的字段在
	if node.Fields.Str("name") != "攻击者" {
		t.Fatalf("钩子该拷 name: %#v", node.Fields)
	}
}

// bind: 改密（覆盖旧哈希）+ 跨渠道拒绝。
func TestAuthBind(t *testing.T) {
	site := authSite(t)
	register := jsonDo(t, site, http.MethodPost, "/api/auth/member/register",
		`{"method":"email","identifier":"a@x.com","secret":"secret123"}`)
	token := sessionToken(t, register.Body.String())

	// 改密
	got := jsonDo(t, site, http.MethodPost, "/api/auth/member/bind",
		`{"method":"email","identifier":"a@x.com","secret":"newsecret1"}`, withBearer(token))
	if got.Code != http.StatusNoContent {
		t.Fatalf("改密 = %d %q", got.Code, got.Body.String())
	}
	// 旧口令登录失败、新口令成功
	old := jsonDo(t, site, http.MethodPost, "/api/auth/member/login",
		`{"method":"email","identifier":"a@x.com","secret":"secret123"}`)
	if old.Code != http.StatusUnauthorized {
		t.Fatalf("旧口令该失效 = %d", old.Code)
	}
	fresh := jsonDo(t, site, http.MethodPost, "/api/auth/member/login",
		`{"method":"email","identifier":"a@x.com","secret":"newsecret1"}`)
	if fresh.Code != http.StatusOK {
		t.Fatalf("新口令该能登录 = %d %q", fresh.Code, fresh.Body.String())
	}
	// 匿名不能 bind
	anon := jsonDo(t, site, http.MethodPost, "/api/auth/member/bind",
		`{"method":"email","identifier":"a@x.com","secret":"whatever1"}`)
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 bind = %d", anon.Code)
	}
	// 渠道不相符（member 的会话去改 staff 的渠道）
	cross := jsonDo(t, site, http.MethodPost, "/api/auth/staff/bind",
		`{"method":"username","identifier":"boss","secret":"whatever1"}`, withBearer(token))
	if cross.Code != http.StatusForbidden {
		t.Fatalf("跨渠道 bind = %d %q", cross.Code, cross.Body.String())
	}
}

// 未注册渠道的会话 ⇒ 匿名（防线在 Actor 解析里, 不是"有会话就认"）。
func TestSessionOfUnregisteredRealmIsAnonymous(t *testing.T) {
	site := authSite(t)
	id, err := site.Engine().RegisterAuth(nil, "member", "email", "z@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	// 手里有库的人自己建了一条"没注册过渠道"的会话
	token, err := site.Engine().CreateSession(nil, "ghost", id)
	if err != nil {
		t.Fatal(err)
	}
	me := jsonDo(t, site, http.MethodGet, "/api/auth/me", "", withBearer(token))
	if me.Code != http.StatusUnauthorized {
		t.Fatalf("未注册渠道该匿名: %d %q", me.Code, me.Body.String())
	}
}

// 渠道声明: 公开可列, 且校验全部 panic（配置错比起不来更糟）。
func TestAuthRealmRegistration(t *testing.T) {
	// /api/auth/realms 公开
	site := authSite(t)
	got := jsonDo(t, site, http.MethodGet, "/api/auth/realms", "")
	if got.Code != http.StatusOK {
		t.Fatalf("列渠道 = %d", got.Code)
	}
	for _, want := range []string{`"name":"member"`, `"name":"staff"`, `"default":true`, `"register_methods":["email"]`} {
		if !strings.Contains(got.Body.String(), want) {
			t.Fatalf("渠道列表缺 %s: %q", want, got.Body.String())
		}
	}
	// 后台登录页要的信息就这些（不含任何凭据信息）
	if strings.Contains(got.Body.String(), "password") || strings.Contains(got.Body.String(), "secret") {
		t.Fatalf("不该泄漏凭据信息: %q", got.Body.String())
	}

	// 各种声明错误 ⇒ panic
	cases := []struct {
		name  string
		apply func(s *Site)
	}{
		{"名字不合法", func(s *Site) { s.Auth().Register(AuthRealm{Name: "Member", NodeType: "member"}) }},
		{"重复", func(s *Site) { s.Auth().Register(AuthRealm{Name: "member", NodeType: "member"}) }},
		{"类型不存在", func(s *Site) { s.Auth().Register(AuthRealm{Name: "ghost", NodeType: "nope"}) }},
		{"类型无认证能力", func(s *Site) {
			s.Auth().Register(AuthRealm{Name: "article", NodeType: "article"})
		}},
		{"方法为空串", func(s *Site) {
			s.Auth().Register(AuthRealm{Name: "x", NodeType: "member", RegisterMethods: []string{""}})
		}},
		{"第二个 default", func(s *Site) {
			s.Auth().Register(AuthRealm{Name: "x", NodeType: "member", Default: true})
		}},
	}
	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			fresh := newAuthSiteForPanic(t)
			defer func() {
				if recover() == nil {
					t.Fatal("该 panic")
				}
			}()
			test.apply(fresh)
		})
	}
	// Setup 之后注册 ⇒ panic
	frozen := authSite(t)
	frozen.Setup()
	defer func() {
		if recover() == nil {
			t.Fatal("Setup 之后注册渠道该 panic")
		}
	}()
	frozen.Auth().Register(AuthRealm{Name: "late", NodeType: "member"})
}

// 类型无认证能力那条用例需要 article 存在（policyTypesYAML 有）—— 单独造一个夹具。
func newAuthSiteForPanic(t *testing.T) *Site {
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
	site.Auth().Register(AuthRealm{Name: "member", NodeType: "member", Default: true})
	return site
}

// SecureCookies(true) ⇒ cookie 带 Secure。
func TestSecureCookies(t *testing.T) {
	site := authSite(t)
	site.SecureCookies(true)
	register := jsonDo(t, site, http.MethodPost, "/api/auth/member/register",
		`{"method":"email","identifier":"a@x.com","secret":"secret123"}`)
	cookies := register.Result().Cookies()
	if len(cookies) == 0 || !cookies[0].Secure {
		t.Fatalf("cookie 该带 Secure: %#v", cookies)
	}
}

// sessionToken 从登录响应里取令牌。
func sessionToken(t *testing.T, body string) string {
	t.Helper()
	var payload struct {
		Token string `json:"token"`
	}
	err := json.Unmarshal([]byte(body), &payload)
	if err != nil || payload.Token == "" {
		t.Fatalf("响应里没有令牌 (%v): %q", err, body)
	}
	return payload.Token
}

// 真实 HTTP 走一遍: 注册 → cookie 登录态 → /me。
func TestAuthOverRealHTTP(t *testing.T) {
	site := authSite(t)
	server := httptest.NewServer(site.Setup())
	defer server.Close()
	client := server.Client()
	// 真是"真机"的关键: 浏览器会存 cookie, http.Client 默认**不存**
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client.Jar = jar

	response, err := client.Post(server.URL+"/api/auth/member/register", "application/json",
		strings.NewReader(`{"method":"email","identifier":"a@x.com","secret":"secret123","fields":{"name":"甲"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("注册 = %d", response.StatusCode)
	}
	// cookie jar 由 client 自己带（真机: 浏览器就是这么干的）
	me, err := client.Get(server.URL + "/api/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	defer me.Body.Close()
	if me.StatusCode != http.StatusOK {
		t.Fatalf("cookie 该带上会话: %d", me.StatusCode)
	}
}
