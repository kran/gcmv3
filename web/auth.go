// 认证 —— **渠道（realm）声明** + 与凭据无关的会话端点。
//
// 渠道是什么: 一条"登录入口"的声明 —— 名字是它的公开标识, 类型是它认哪种账号,
// RegisterMethods 说明它开不开自助注册, Default 是登录界面默认选谁。
//
// 渠道**不承载**凭据机制（口令在 password.go, 微信/SMS/SSO 由插件挂自己的路由）,
// 也**不承载**权限（"能不能进后台"看节点上的角色 —— 登录之后再说）。
//
//	site.Auth().Register(AuthRealm{Name: "staff", NodeType: "staff", Default: true})
//
// 就是这一行让后台开箱能登录: 口令是框架地基, 不是插件。
package web

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/kran/cho"
	"github.com/kran/gcmv3/core"
)

// HookAuthRegister 注册落库前的钩子: 站点校验/补默认值。
// 签名: func(*CmsCtx, AuthRealm, *RegisterInput, *core.Node) error
const HookAuthRegister = "web.auth_register"

// RegisterInput 注册请求（凭据机制中立的输入）。
//
// **Fields 不会自动进节点** —— 它只是钩子的输入, 由站点钩子显式拷贝它信任的那些字段。
// 这不是洁癖: roles 是能力注入的字段, 合法值里永远含 owner/admin（系统角色并入词表,
// 实测过）⇒ 自动落库的话, 一个公开的注册端点就能造出 owner。
//
// 钩子里的写法（白名单拷贝。注意别把**没提交**的字段拷成 nil —— 字段校验会拒绝）:
//
//	site.Hook(web.HookAuthRegister, func(_ *web.CmsCtx, realm web.AuthRealm,
//		in *web.RegisterInput, node *core.Node) error {
//		for _, name := range []string{"name", "phone"} {
//			if value, ok := in.Fields[name]; ok && value != nil {
//				node.Fields[name] = value
//			}
//		}
//		return nil
//	})
type RegisterInput struct {
	Method     string         `json:"method"`
	Identifier string         `json:"identifier"`
	Secret     string         `json:"secret"`
	Fields     map[string]any `json:"fields"`
}

func defineAuthHooks(engine core.Engine) {
	err := engine.Hooks().Define(map[string]any{
		HookAuthRegister: func(*CmsCtx, AuthRealm, *RegisterInput, *core.Node) error { return nil },
	})
	if err != nil {
		panic("web: define auth hooks: " + err.Error())
	}
}

// AuthRealm 一条登录渠道。
type AuthRealm struct {
	Name     string `json:"name"`
	NodeType string `json:"node_type"`
	// RegisterMethods 允许**自助注册**的登录方式（如 ["email","phone"]）。
	// 空 = 不开放注册（员工/管理员这类由迁移或 CLI 创建）。一个字段同时回答
	// "开不开注册"与"能用哪些方式注册" —— 分成两个字段就会自相矛盾。
	RegisterMethods []string `json:"register_methods,omitempty"`
	// Default 登录界面默认选哪个渠道（最多一个）。
	Default bool `json:"default,omitempty"`
}

// AuthRegistry 站点的渠道声明（配置期注册; 启动后冻结）。
type AuthRegistry struct {
	site   *Site
	realms map[string]AuthRealm
	// auto 哪些渠道是**框架自动注册**的（每个 auth 能力类型一条同名渠道）——
	// 站点显式 Register 会覆盖它们; 两条显式渠道重名仍然 panic。
	auto         map[string]bool
	defaultRealm string
}

var authRealmName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func newAuthRegistry(site *Site) *AuthRegistry {
	return &AuthRegistry{site: site, realms: map[string]AuthRealm{}, auto: map[string]bool{}}
}

// registerDefaults 给每个声明了 authentication 能力的类型自动注册一条**同名渠道**。
//
// 为什么这样做: "类型声明了能登录" 是站点的明确意图（authentication 词表就在
// site.yaml 里），那么"有一条能用的登录入口"是自然推论 —— 否则类型声明了却登不
// 进来（每个站点都要记得写一遍 Auth().Register, 漏了就是"后台登录不了"）。
//
// 站点显式 Register 会**覆盖**它（补 RegisterMethods / Default 等）; 名字 = 类型名,
// 一个概念一个名字（/api/auth/staff/login 一眼知道是谁）。
func (r *AuthRegistry) registerDefaults() {
	for _, typeName := range r.site.types.AuthTypes() {
		r.realms[typeName] = AuthRealm{Name: typeName, NodeType: typeName}
		r.auto[typeName] = true
	}
}

// Register 声明一条渠道。配置错了就 panic: 带着一条含糊的认证映射启动, 比起不来更糟。
func (r *AuthRegistry) Register(realm AuthRealm) {
	if r.site.started {
		panic("web: auth.Register(" + realm.Name + "): must be registered before Setup")
	}
	if !authRealmName.MatchString(realm.Name) {
		panic(fmt.Sprintf("web: auth realm name %q must match %s", realm.Name, authRealmName))
	}
	if _, exists := r.realms[realm.Name]; exists && !r.auto[realm.Name] {
		panic("web: duplicate auth realm " + realm.Name)
	}
	delete(r.auto, realm.Name) // 覆盖掉自动那条 ⇒ 之后重名就是真重名
	def, ok := r.site.types.Type(realm.NodeType)
	if !ok {
		panic(fmt.Sprintf("web: auth realm %q: node type %q not defined", realm.Name, realm.NodeType))
	}
	if def.Capabilities.Authentication == nil {
		panic(fmt.Sprintf("web: auth realm %q: node type %q has no authentication capability",
			realm.Name, realm.NodeType))
	}
	seen := map[string]bool{}
	for _, method := range realm.RegisterMethods {
		if method == "" || seen[method] {
			panic(fmt.Sprintf("web: auth realm %q: register methods must be non-empty and unique", realm.Name))
		}
		seen[method] = true
		// 自助注册不能开一个"框架不管的口令方式": 声明在类型的 authentication.methods
		// 里才算框架管（否则注册出来的凭据没人核验, 用户以为自己注册成功了）。
		if !r.site.types.HasAuthMethod(realm.NodeType, method) {
			panic(fmt.Sprintf(
				"web: auth realm %q: register method %q 未声明在类型 %q 的 authentication.methods 里",
				realm.Name, method, realm.NodeType))
		}
	}
	if realm.Default && r.defaultRealm != "" {
		panic(fmt.Sprintf("web: auth realms %q and %q are both default", r.defaultRealm, realm.Name))
	}
	r.realms[realm.Name] = realm
	if realm.Default {
		r.defaultRealm = realm.Name
	}
}

// Realms 已注册渠道（按名字排序, 输出稳定）—— 登录界面用它列出可登录的渠道。
func (r *AuthRegistry) Realms() []AuthRealm {
	out := make([]AuthRealm, 0, len(r.realms))
	for _, realm := range r.realms {
		out = append(out, realm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *AuthRegistry) realm(name string) (AuthRealm, bool) {
	realm, ok := r.realms[name]
	return realm, ok
}

// setupAuth 挂 /api/auth（Setup 的第三步半 —— 在 API 之前, 都在 /api 前缀下）。
func (s *Site) setupAuth(g *cho.Cho[*CmsCtx]) {
	g.Group("/auth", func(ag *cho.Cho[*CmsCtx]) {
		ag.Get("/realms", s.authRealms)
		ag.Get("/me", s.authMe)
		ag.Post("/logout", s.authLogout)
		ag.Post("/{realm}/login", s.authLogin)
		ag.Post("/{realm}/register", s.authRegister)
		ag.Post("/{realm}/bind", s.authBind)
	})
}

// authRealms GET /api/auth/realms —— 公开: 列渠道（不含任何凭据信息）。
func (s *Site) authRealms(ctx *CmsCtx) {
	realms := s.auth.Realms()
	// site 也在这里给: 这是**公开**端点, 而登录页在**登录前**就要显示站点名
	// （登录响应与 /api/auth/me 里也有 —— 三处一致）。
	_ = ctx.Json(http.StatusOK, map[string]any{"site": s.name, "realms": realms})
}

// authMe GET /api/auth/me —— 当前身份 + 掩码后的节点（键名与 /api/nodes/* 一致: node）。
func (s *Site) authMe(ctx *CmsCtx) {
	if ctx.Actor().IsAnonymous() {
		ctx.Fail(Unauthorized("未登录"))
		return
	}
	user, err := ctx.Principal()
	if err != nil {
		ctx.Fail(Unauthorized("未登录"))
		return
	}
	masked, err := ctx.MaskNode(user)
	if err != nil {
		ctx.Fail(err)
		return
	}
	_ = ctx.Json(http.StatusOK, map[string]any{
		"actor": ctx.Actor(), "node": masked, "site": s.name,
	})
}

// authLogout POST /api/auth/logout —— **与渠道无关**: 一个浏览器一个会话。
func (s *Site) authLogout(ctx *CmsCtx) {
	token := ctx.authToken()
	if token != "" {
		err := s.engine.DeleteSession(ctx.DB(), token)
		if err != nil {
			ctx.Fail(err)
			return
		}
	}
	http.SetCookie(ctx.W, &http.Cookie{
		Name: TokenCookie, Value: "", Path: "/",
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	_ = ctx.NoContent(http.StatusNoContent)
}

// authRealmOf 路径上的渠道（没注册 ⇒ 404 —— 渠道名是公开的, 不是秘密）。
func (s *Site) authRealmOf(ctx *CmsCtx) (AuthRealm, error) {
	realm, ok := s.auth.realm(ctx.PathValue("realm"))
	if !ok {
		return AuthRealm{}, NotFound("渠道不存在")
	}
	return realm, nil
}

// AuthSession 建会话 + 下发 cookie + 装上身份, 返回令牌。
//
// **不写响应** —— 调用方决定回什么（内置口令回 {token, actor, user}; 插件可以只回自己
// 那一套）。凭据机制共用它, 于是"会话怎么建、cookie 长什么样"只有一处。
func (c *CmsCtx) AuthSession(realm AuthRealm, nodeID int64) (string, error) {
	configured, ok := c.site.auth.realm(realm.Name)
	if !ok || configured.NodeType != realm.NodeType {
		return "", Forbidden("渠道 %q 未注册", realm.Name)
	}
	node, err := c.site.engine.GetNode(nodeID)
	if err != nil {
		return "", CoreError(err)
	}
	if node == nil || node.Type != realm.NodeType {
		return "", Forbidden("节点不属于渠道 %q", realm.Name)
	}
	token, err := c.site.engine.CreateSession(c.DB(), realm.Name, nodeID)
	if err != nil {
		return "", CoreError(err)
	}
	// SameSite=Lax: 跨站表单 POST 不带这个 cookie（CSRF 的第一道防线）。
	// 更强的（double-submit token / 自定义头）留到后台那一步再定。
	http.SetCookie(c.W, &http.Cookie{
		Name: TokenCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: c.site.secureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(core.SessionTTL),
	})
	c.SetActor(Actor{NodeID: node.ID, NodeType: node.Type, Realm: realm.Name})
	c.principal = node
	c.principalLoaded = true
	return token, nil
}

// respondLogin 登录/注册/绑定成功后的统一响应: 令牌 + 身份 + 掩码后的节点。
//
// user 过读规则（与 GET /api/auth/me 同一套）——站点忘了给这个类型注册读规则 ⇒
// 这里当场报错（fail-loud, 而不是回一个字段全裸的 user）。
func (c *CmsCtx) respondLogin(token string) error {
	user, err := c.Principal()
	if err != nil {
		return err
	}
	masked, err := c.MaskNode(user)
	if err != nil {
		return err
	}
	return c.Json(http.StatusOK, map[string]any{
		"token": token, "actor": c.Actor(), "node": masked, "site": c.site.name,
	})
}
