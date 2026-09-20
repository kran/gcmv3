// Package web 授权 —— 一个类型一份策略, **四个动词各一个回调**（CRUD 就四个动词, 不发明第五个）。
//
//	site.Type("article").OnRead(...).OnUpdate(...)
//
// 语义:
//
//	OnRead   (where, hide)  产出**行范围**（必须显式产出, 见下）+ 声明"这些角色看不到
//	                        这些字段"（不碰 = 全部可见; 只影响输出, 不影响筛选/排序）
//	OnCreate (node, allow)  身份判断 + 声明**客户端可写**的字段 + 就地加工（补 author、
//	                        改状态）; 规则自己补的字段不参与白名单
//	OnUpdate (node, patch, allow)  同上（node = 当前状态, 原始未掩码）
//	OnDelete (node)         纯身份判断（删除没有字段面）
//
// 返回 error = 拒绝。**想指定状态码就返回 *Error**（web.Unauthorized / Forbidden /
// Conflict …）; 返回别的 error 视为服务端错误（500 + 日志）—— 规则里的数据库错误、
// 拼错的方法调用不该伪装成 403 把真问题埋掉。
//
// **默认是拒绝**: 没注册规则的写动作一律 401/403; 没注册 OnRead 的类型读不出来。
// 注册一条规则就接管了默认（"要看什么就显式写出来"）。
//
// 为什么不是"每个（类型 × 动作）一个事件": 那种做法拿一个 key 一个订阅者的广播机制装
// 一次性回调, 于是要拼事件名、per-type define 循环、运行期签名校验 —— 全是机制税。
// 这里就是一张 map + 四个类型安全的函数类型（写错编译期就报）。
//
// 自定义读场景（"我的内容"这类）**不在这里扩展**: 站点自己的端点把条件拼进 where
// 再调 List —— 策略范围与客户端条件只会 AND 收窄, 站点多 AND 一项同样是收窄, 不越权。
package web

import (
	"fmt"
	"slices"
	"sort"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// ReadRule 读规则。必须给 where 赋值（**显式**"全不限"用 so.P("true")）——
// 规则跑完还是零值 = 配置错误, 不是"默认全允许"。
type ReadRule func(c *CmsCtx, where *so.Where, hide *Grant) error

// CreateRule 创建规则。客户端提交了哪些字段在规则跑之前已快照, 规则自己补的字段
// 不参与白名单（补 author / 补状态是规则的权利, 不是客户端的）。
type CreateRule func(c *CmsCtx, node *core.Node, allow *Grant) error

// UpdateRule 更新规则。patch 里是客户端提交的差量; node 是**这个节点的当前状态**
// （原始节点, 未经掩码）—— 可写性常常取决于它的字段（"已发布的内容只有管理角色能改"）,
// 而写入口本来就已经查过它了, 所以直接给, 规则不用自己再查一遍。
type UpdateRule func(c *CmsCtx, node *core.Node, patch *core.NodePatch, allow *Grant) error

// DeleteRule 删除规则（只有身份判断; node 同 UpdateRule —— 当前状态）。
type DeleteRule func(c *CmsCtx, node *core.Node) error

// Policy 一个类型的策略。零值 = 全拒。
type Policy struct {
	OnRead   ReadRule
	OnCreate CreateRule
	OnUpdate UpdateRule
	OnDelete DeleteRule
}

// TypePolicy 注册句柄（site.Type("article") 返回它）。
type TypePolicy struct {
	site   *Site
	name   string
	policy Policy
}

// Type 取某个类型的策略句柄（配置期用）。
//
// 类型名写错立刻 panic —— 绝不"顺手建一个不存在的类型"（那会让拼错的策略静默永不生效）。
// Setup 之后再注册同样 panic: 策略在开始服务前冻结（运行期改授权只有重启一条路, 免得
// "某些请求用旧策略、某些用新策略"这种没法复现的状态）。
func (s *Site) Type(typeName string) *TypePolicy {
	if s.started {
		panic("web: site.Type(" + typeName + "): policies must be registered before Setup")
	}
	if _, ok := s.types.Type(typeName); !ok {
		panic("web: site.Type(" + typeName + "): type not defined")
	}
	if s.policies == nil {
		s.policies = make(map[string]*TypePolicy, 8)
	}
	if handle, ok := s.policies[typeName]; ok {
		return handle
	}
	handle := &TypePolicy{site: s, name: typeName}
	s.policies[typeName] = handle
	return handle
}

// policy 取某类型已注册的策略（没注册 = nil）。
func (s *Site) policy(typeName string) *Policy {
	handle, ok := s.policies[typeName]
	if !ok {
		return nil
	}
	return &handle.policy
}

func (t *TypePolicy) OnRead(rule ReadRule) *TypePolicy {
	if rule == nil {
		panic("web: " + t.name + ".OnRead: rule is nil")
	}
	t.policy.OnRead = rule
	return t
}

func (t *TypePolicy) OnCreate(rule CreateRule) *TypePolicy {
	if rule == nil {
		panic("web: " + t.name + ".OnCreate: rule is nil")
	}
	t.policy.OnCreate = rule
	return t
}

func (t *TypePolicy) OnUpdate(rule UpdateRule) *TypePolicy {
	if rule == nil {
		panic("web: " + t.name + ".OnUpdate: rule is nil")
	}
	t.policy.OnUpdate = rule
	return t
}

func (t *TypePolicy) OnDelete(rule DeleteRule) *TypePolicy {
	if rule == nil {
		panic("web: " + t.name + ".OnDelete: rule is nil")
	}
	t.policy.OnDelete = rule
	return t
}

// readRule 求某类型的读规则: 行范围 + 这次看不到的字段。
//
// 同一请求内按类型只求一次（结果是当前 Actor 的函数, 而 Actor 与 CmsCtx 同生命周期）
// —— 所以 SetActor 换身份时必须作废缓存。
func (c *CmsCtx) readRule(typeName string) (so.Where, []string, error) {
	if got, ok := c.readRules[typeName]; ok {
		return got.where, got.hidden, got.err
	}
	where, hidden, err := c.resolveReadRule(typeName)
	if c.readRules == nil {
		c.readRules = make(map[string]readRuleResult, 4)
	}
	c.readRules[typeName] = readRuleResult{where: where, hidden: hidden, err: err}
	return where, hidden, err
}

type readRuleResult struct {
	where  so.Where
	hidden []string
	err    error
}

func (c *CmsCtx) resolveReadRule(typeName string) (so.Where, []string, error) {
	def, ok := c.site.types.Type(typeName)
	if !ok {
		return so.Where{}, nil, fmt.Errorf("web: policy: type %q not defined", typeName)
	}
	policy := c.site.policy(typeName)
	if policy == nil || policy.OnRead == nil {
		// 没注册 = 不开放（默认拒绝, 而不是"默认全公开"）
		return so.Where{}, nil, Forbidden("该类型的读取未开放")
	}
	var where so.Where
	hide := newGrant()
	err := policy.OnRead(c, &where, hide)
	if err != nil {
		return so.Where{}, nil, err
	}
	if where.IsZero() {
		// 零值 = "忘了给范围", 不是"全不限" —— 后者必须显式 so.P("true")
		return so.Where{}, nil, fmt.Errorf(
			"web: OnRead for type %q produced no scope (use so.P(\"true\") to allow every row)",
			typeName)
	}
	err = c.validateGrant(typeName, hide)
	if err != nil {
		return so.Where{}, nil, err
	}
	// 按声明顺序算"哪些字段看不到"（输出稳定, 与 Grant 内部的 map 顺序无关）
	roles := c.Actor().Roles
	var hidden []string
	for _, field := range def.Fields {
		if hide.Has(roles, field.Name) {
			hidden = append(hidden, field.Name)
		}
	}
	return where, hidden, nil
}

// validateGrant 校验规则里写的字段名与角色名 —— 拼错就报错。
//
// 为什么必须响亮: 读侧拼错字段 = 本该隐藏的没隐藏（泄漏）; 写侧拼错 = 授权了个不存在的
// 字段（静默无效, 而站点以为放行了）。两种都不能靠"看起来没事"蒙过去。
func (c *CmsCtx) validateGrant(typeName string, g *Grant) error {
	for _, name := range g.names() {
		_, ok := c.site.types.Field(typeName, name)
		if !ok {
			return fmt.Errorf("web: policy for type %q uses undeclared field %q", typeName, name)
		}
	}
	for _, role := range g.roles() {
		if !c.site.validRole(role) {
			return fmt.Errorf("web: policy for type %q uses unknown role %q", typeName, role)
		}
	}
	return nil
}

// validRole 站点级的角色词汇: 系统角色（owner/admin）+ public + **所有**类型名 +
// 所有 authentication 词表里的词。
//
// 为什么是站点级而不是"这个类型自己的词表": 规则里提到别的类型的角色是完全正常的
// （article 的规则要提到 staff / 秘书处）。打错字照样会被拦住。
func (s *Site) validRole(role string) bool {
	switch role {
	case types.RoleOwner, types.RoleAdmin, types.RolePublic:
		return true
	}
	for name, def := range s.types.Defs() {
		if role == name {
			return true
		}
		capability := def.Capabilities.Authentication
		if capability != nil && slices.Contains(capability.Roles, role) {
			return true
		}
	}
	return false
}

// ── 写侧求值 ──
//
// 三个 authorize* 只做"策略这一步": 身份 + 白名单 + 就地加工。落库由写入口做, 且在
// 它们之前已经**按读范围查过节点**（看得到才改得到 —— 见 write.go）。

// authorizeCreate 过创建规则 + 白名单。node.Type 必须有已注册的 OnCreate。
func (c *CmsCtx) authorizeCreate(node *core.Node) error {
	if node == nil || node.Type == "" {
		return BadRequest("创建需要类型")
	}
	err := c.requireRule(node.Type, VerbCreate)
	if err != nil {
		return err
	}
	policy := c.site.policy(node.Type)
	// 客户端提交的字段在规则加工前快照（规则自己补的字段不参与白名单）
	submitted := fieldNames(node.Fields)
	allow := newWriteGrant()
	err = policy.OnCreate(c, node, allow)
	if err != nil {
		return err
	}
	return c.checkWritable(node.Type, submitted, allow)
}

// authorizeUpdate 过更新规则 + 白名单。node 是已按读范围查到的现存节点（原始未掩码）。
func (c *CmsCtx) authorizeUpdate(node *core.Node, patch *core.NodePatch) error {
	if node == nil {
		return NotFound("不存在")
	}
	if patch == nil {
		return BadRequest("更新需要差量")
	}
	err := c.requireRule(node.Type, VerbUpdate)
	if err != nil {
		return err
	}
	policy := c.site.policy(node.Type)
	submitted := fieldNames(patch.Fields)
	allow := newWriteGrant()
	err = policy.OnUpdate(c, node, patch, allow)
	if err != nil {
		return err
	}
	return c.checkWritable(node.Type, submitted, allow)
}

// authorizeDelete 过删除规则。node 是已按读范围查到的现存节点。
func (c *CmsCtx) authorizeDelete(node *core.Node) error {
	if node == nil {
		return NotFound("不存在")
	}
	err := c.requireRule(node.Type, VerbDelete)
	if err != nil {
		return err
	}
	return c.site.policy(node.Type).OnDelete(c, node)
}

// rolesOnlyOwner roles 是**提权面**（它决定谁是 owner/admin）: 只有 owner 能写。
//
// 这是框架不变量, 不是站点规则 —— roles 字段本身就由框架按 authentication 能力注入
// （见 types.injectAuthRoles），所以"谁能改它"也该由框架兜住: 每个站点各写一遍,
// 漏一次就是提权洞（admin 给自己加 owner）。
//
// 实现放在校验点而不是"从 grant 里删掉": 站点可能给管理角色授了 "*", 而 v3 没有
// deny 语义（故意不引入优先级地狱）—— 在提交字段这一步拦掉, "*" 也绕不过去。
func (c *CmsCtx) rolesOnlyOwner(typeName, field string) bool {
	if field != types.RolesField {
		return false
	}
	if _, ok := c.site.types.AuthMethods(typeName); !ok {
		return false // 该类型没有 roles 字段（不是 auth 类型）
	}
	return !c.Actor().IsOwner()
}

// checkWritable 白名单: 客户端提交的字段必须**既可写又可读**。
//
//	可写 —— 规则把它（或 "*"）授予了当前角色; 一个都没授予 = 没有授权这个动作（403）
//	可读 —— 读规则藏起来的字段连写也不行（可写 ⊆ 可读; 否则能靠"写进去再读出来"绕过掩码）
//
// 拒绝是 422 + 字段级 details, 不是 403: 403 说的是"这个动作不行", 这里是"动作行、
// 但你提交的这几个字段不行"。
func (c *CmsCtx) checkWritable(typeName string, submitted []string, allow *Grant) error {
	roles := c.Actor().Roles
	if !allow.anyFor(roles) {
		return Forbidden("没有授权这个动作（规则没有授予任何字段）")
	}
	_, hidden, err := c.readRule(typeName)
	if err != nil {
		return err
	}
	var rejected map[string]string
	for _, name := range submitted {
		switch {
		case slices.Contains(hidden, name):
			rejected = addRejected(rejected, name, "对当前身份不可见")
		case c.rolesOnlyOwner(typeName, name):
			rejected = addRejected(rejected, name, "只有 owner 能改角色")
		case !allow.Has(roles, name):
			rejected = addRejected(rejected, name, "对当前身份不可写")
		}
	}
	if len(rejected) > 0 {
		return InvalidFields(rejected)
	}
	return nil
}

// Verb 三个写动作。读只有一个动词（OnRead），不走这里。
type Verb string

const (
	VerbCreate Verb = "create"
	VerbUpdate Verb = "update"
	VerbDelete Verb = "delete"
)

// registered 这个类型注册了该动词的规则吗。
//
// **写入口在取节点之前先问这个** —— 否则"未注册写规则的类型"会变成"这个 id 存不存在"
// 的探测器（匿名打一个不存在的 id 拿到 404、存但无权的 id 拿到 401/403）。
func (p *Policy) registered(verb Verb) bool {
	if p == nil {
		return false
	}
	switch verb {
	case VerbCreate:
		return p.OnCreate != nil
	case VerbUpdate:
		return p.OnUpdate != nil
	case VerbDelete:
		return p.OnDelete != nil
	}
	return false
}

// requireRule 没注册该动词的规则 ⇒ 拒绝（匿名 401 / 已认证 403）。
func (c *CmsCtx) requireRule(typeName string, verb Verb) error {
	if c.site.policy(typeName).registered(verb) {
		return nil
	}
	message := fmt.Sprintf("类型 %q 不允许 %s", typeName, verb)
	if c.Actor().IsAnonymous() {
		return Unauthorized("%s", message)
	}
	return Forbidden("%s", message)
}

// fieldNames 提交的字段名（排序输出 —— 白名单判断与 details 都不该依赖 map 顺序）。
func fieldNames(fields map[string]any) []string {
	if len(fields) == 0 {
		return nil
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func addRejected(acc map[string]string, name, reason string) map[string]string {
	if acc == nil {
		acc = map[string]string{}
	}
	acc[name] = reason
	return acc
}
