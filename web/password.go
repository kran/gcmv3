// 口令认证 —— **框架内置**, 不是插件。
//
// 为什么内置: "后台开箱能登录"是框架的责任, 而口令是唯一零外部依赖的凭据机制。
// 插件机制留给**站点特有的外部身份源**（微信 / 短信 / SSO / OAuth）—— 那些只有站点
// 知道怎么接。站点要开后台只需声明一条渠道:
//
//	site.Auth().Register(AuthRealm{Name: "staff", NodeType: "staff", Default: true})
//
// 三个端点:
//
//	POST /api/auth/{realm}/login     {method, identifier, secret}
//	POST /api/auth/{realm}/register  仅当 method 在该渠道的 RegisterMethods 里
//	POST /api/auth/{realm}/bind      改密/绑凭据（需已登录且渠道相符）
//
// 口令落 auth_methods.data = {"password": "<bcrypt hash>"} —— 内核不解释 data。
package web

import (
	"errors"
	"net/http"
	"slices"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/kran/gcmv3/core"
)

const (
	// minPasswordLength 口令最短长度（按**字符**算, 中文口令不该被按字节判）。
	minPasswordLength = 8
	// passwordKey auth_methods.data 里的键名 —— 这个键由本文件解释, 内核不碰。
	passwordKey = "password"
)

// hashPassword 生成口令哈希（bcrypt, DefaultCost）。
//
// bcrypt 而不是 argon2id: 参数少（没有要调的 salt/内存/迭代）, DefaultCost 就是合理
// 默认, 且自带 per-hash salt 与工作因子升级路径（校验时读 hash 里的 cost）。
func hashPassword(secret string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// verifyPassword 校验口令。**不区分**"没有这条凭据"与"口令不对" —— 调用方统一回
// 一样的 401（否则登录端点成了账号枚举器）。
func verifyPassword(method *core.AuthMethod, secret string) bool {
	if method == nil {
		return false
	}
	hash := method.Data.Str(passwordKey)
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)) == nil
}

// authLogin POST /api/auth/{realm}/login
//
// 任何失败（渠道不认这个方式 / 没这条凭据 / 口令不对）都是同一个 401。
//
// **已知缺口: 没有登录限速** —— 爆破只能靠部署层（nginx limit_req / WAF）挡。
// 要自己做的话最简形态是进程内滑动窗口（按 渠道+标识+IP 计数, 超限 429 + Retry-After）,
// 多实例各算各的。
func (s *Site) authLogin(ctx *CmsCtx) {
	realm, err := s.authRealmOf(ctx)
	if err != nil {
		ctx.Fail(err)
		return
	}
	var input struct {
		Method     string `json:"method"`
		Identifier string `json:"identifier"`
		Secret     string `json:"secret"`
	}
	err = ctx.BindJSON(&input)
	if err != nil {
		ctx.Fail(err)
		return
	}
	if input.Method == "" || input.Identifier == "" || input.Secret == "" {
		ctx.Fail(BadRequest("method / identifier / secret 必填"))
		return
	}
	// 口令登录只认**类型声明过**的口令类方式（authentication.methods）。
	//
	// 不校验的后果是个后门: 别的登录机制（微信那种）的凭据里一旦被写进 password,
	// 拿它当 (方法, 标识, 口令) 就能走口令登录 —— 那条凭据本该只走插件自己的核验。
	if !s.types.HasAuthMethod(realm.NodeType, input.Method) {
		ctx.Fail(BadRequest("类型 %q 不支持口令方式 %q（见 types.yaml 的 authentication.methods）",
			realm.NodeType, input.Method))
		return
	}
	key := attemptKey("login", realm.Name, input.Method, input.Identifier)
	if ctx.limited(key) {
		return
	}
	method, err := s.engine.FindAuth(realm.NodeType, input.Method, input.Identifier)
	if err != nil {
		ctx.Fail(err)
		return
	}
	verified := verifyPassword(method, input.Secret)
	ctx.recordAttempt(key, verified)
	if !verified {
		ctx.Fail(Unauthorized("账号或口令不正确"))
		return
	}
	token, err := ctx.AuthSession(realm, method.NodeID)
	if err != nil {
		ctx.Fail(err)
		return
	}
	err = ctx.respondLogin(token)
	if err != nil {
		ctx.Fail(err)
		return
	}
}

// authRegister POST /api/auth/{realm}/register
//
// 只有该渠道的 RegisterMethods 里声明过的方式能自助注册（空 = 不开放）。节点由
// **站点钩子**填字段 —— 客户端提交的 Fields 不自动落库（否则能塞 roles 提权）。
func (s *Site) authRegister(ctx *CmsCtx) {
	realm, err := s.authRealmOf(ctx)
	if err != nil {
		ctx.Fail(err)
		return
	}
	if len(realm.RegisterMethods) == 0 {
		ctx.Fail(Forbidden("渠道 %q 不开放注册", realm.Name))
		return
	}
	var input RegisterInput
	err = ctx.BindJSON(&input)
	if err != nil {
		ctx.Fail(err)
		return
	}
	if !slices.Contains(realm.RegisterMethods, input.Method) {
		ctx.Fail(Forbidden("渠道 %q 不允许用 %q 注册", realm.Name, input.Method))
		return
	}
	// 注册也限速: 否则 409（标识已被注册）会变成账号枚举器（慢慢试也能枚举）。
	registerKey := attemptKey("register", realm.Name, input.Method, input.Identifier)
	if ctx.limited(registerKey) {
		return
	}
	if input.Identifier == "" {
		ctx.Fail(BadRequest("identifier 必填"))
		return
	}
	err = checkPassword(input.Secret)
	if err != nil {
		ctx.Fail(err)
		return
	}
	// 节点从一个**空字段**的节点开始, 字段由钩子决定（见 RegisterInput 的注释）
	node := &core.Node{Type: realm.NodeType, Fields: core.Fields{}}
	err = s.engine.Hooks().Fire(HookAuthRegister, ctx, realm, &input, node)
	if err != nil {
		ctx.Fail(err)
		return
	}
	hash, err := hashPassword(input.Secret)
	if err != nil {
		ctx.Fail(err)
		return
	}
	data := core.Fields{passwordKey: hash}
	id, err := s.engine.RegisterAuth(ctx.DB(), realm.NodeType, input.Method, input.Identifier, data, node)
	if err != nil {
		ctx.recordAttempt(registerKey, !errors.Is(err, core.ErrDuplicate))
		if errors.Is(err, core.ErrDuplicate) {
			// 撞上了唯一约束: 这个标识已经有人注册过（注册端点必须能告诉用户, 所以
			// 这里是有意的账号存在性泄漏; 限速让枚举变慢）。
			ctx.Fail(Conflict("该 %s 已被注册", input.Method))
			return
		}
		ctx.Fail(err)
		return
	}
	token, err := ctx.AuthSession(realm, id)
	if err != nil {
		ctx.Fail(err)
		return
	}
	err = ctx.respondLogin(token)
	if err != nil {
		ctx.Fail(err)
		return
	}
}

// authBind POST /api/auth/{realm}/bind —— 给当前身份写一条凭据（改密 / 首次绑口令）。
//
// 要求已登录、且当前身份的渠道与路径上的渠道一致（拿 A 渠道的会话去改 B 渠道的凭据 ✗）。
func (s *Site) authBind(ctx *CmsCtx) {
	realm, err := s.authRealmOf(ctx)
	if err != nil {
		ctx.Fail(err)
		return
	}
	actor := ctx.Actor()
	if actor.IsAnonymous() {
		ctx.Fail(Unauthorized("未登录"))
		return
	}
	if actor.NodeType != realm.NodeType || actor.Realm != realm.Name {
		ctx.Fail(Forbidden("当前身份不属于渠道 %q", realm.Name))
		return
	}
	var input struct {
		Method     string `json:"method"`
		Identifier string `json:"identifier"`
		Secret     string `json:"secret"`
	}
	err = ctx.BindJSON(&input)
	if err != nil {
		ctx.Fail(err)
		return
	}
	if input.Method == "" || input.Identifier == "" {
		ctx.Fail(BadRequest("method / identifier 必填"))
		return
	}
	if !s.types.HasAuthMethod(realm.NodeType, input.Method) {
		ctx.Fail(BadRequest("类型 %q 不支持口令方式 %q（见 types.yaml 的 authentication.methods）",
			realm.NodeType, input.Method))
		return
	}
	err = checkPassword(input.Secret)
	if err != nil {
		ctx.Fail(err)
		return
	}
	hash, err := hashPassword(input.Secret)
	if err != nil {
		ctx.Fail(err)
		return
	}
	data := core.Fields{passwordKey: hash}
	// SetAuthMethod: 已经有这条 (方式, 标识) 就覆盖哈希, 否则新建一条。
	err = s.engine.SetAuthMethod(ctx.DB(), realm.NodeType, actor.NodeID, input.Method, input.Identifier, data)
	if err != nil {
		ctx.Fail(err)
		return
	}
	_ = ctx.NoContent(http.StatusNoContent)
}

// checkPassword 口令长度（**字符**数, 不是字节数 —— 中文口令按字节判会莫名变严）。
//
// 长度是唯一的口令策略: 复杂度规则（大小写+数字+符号）实测把人推向 "Passw0rd!" 这类
// 弱口令, 不如给一个足够长的下限, 剩下的交给限速（见 authLogin 的已知缺口）。
func checkPassword(secret string) error {
	if utf8.RuneCountInString(secret) < minPasswordLength {
		return Errorf(http.StatusUnprocessableEntity, "口令至少 %d 个字符", minPasswordLength)
	}
	return nil
}
