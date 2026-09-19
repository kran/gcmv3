// 身份: 这次请求是谁。
//
// **匿名 = NodeID == 0** —— 没有 Kind 枚举: 匿名不是"另一种身份", 就是"没有身份"。
// 需要判断的地方直接看 NodeID（或 IsAnonymous()）即可。
//
// Actor 是**值**: 一次请求解析一次, 之后不再变（权限求值读它）。它不是业务实体的
// 副本 —— 需要节点本身用 Principal()。
package web

import (
	"errors"
	"slices"
	"strings"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/types"
)

// TokenCookie 会话令牌的 cookie 名（前台页面用; API 客户端走 Authorization）。
// 两个来源等价 —— 双轨只是为了"浏览器里有 cookie、脚本里有 header"两种场景都顺手。
const TokenCookie = "gcm_token"

// Actor 请求身份。
type Actor struct {
	NodeID   int64  `json:"node_id,omitempty"`
	NodeType string `json:"node_type,omitempty"` // 已认证节点的类型
	Realm    string `json:"realm,omitempty"`     // 会话绑定的 realm（不透明字符串）
	// Roles 词的**来源有两处**（这一版先这样）:
	//
	//	节点的类型名   —— "是不是会员"就是类型本身（member / staff …）
	//	节点 roles 字段 —— 能力注入的那个多选字段（owner / admin / 站点自定义词表）
	//
	// 说实话"角色"这个概念内涵有点杂（身份类型与权限词表混在一起）; 为了简单先这样。
	Roles []string `json:"roles,omitempty"`
}

// IsAnonymous 没有身份。
func (a Actor) IsAnonymous() bool { return a.NodeID == 0 }

// HasRole 是否持有某角色（包含类型名那一份）。
func (a Actor) HasRole(name string) bool { return slices.Contains(a.Roles, name) }

// ErrNoPrincipal 当前身份没有对应的节点（匿名）。
var ErrNoPrincipal = errors.New("web: actor has no node principal")

// Actor 当前请求身份（懒解析一次, 之后用缓存的值）。
//
// 解析链: cookie 或 Authorization: Bearer → 会话 → 节点 → 角色。
// **任何一步不成立都退回匿名**（令牌错/过期/节点没了 → 匿名, 而不是 500 或 401 ——
// 要不要 401 是各个入口自己的事; 匿名身份本身是合法状态）。
func (c *CmsCtx) Actor() Actor {
	if c.actorLoaded {
		return c.actor
	}
	c.actorLoaded = true
	c.actor = Actor{}

	token := c.authToken()
	if token == "" {
		return c.actor
	}
	session, err := c.site.engine.ValidSession(token)
	if err != nil || session == nil {
		return c.actor
	}
	node, err := c.site.engine.GetNode(session.NodeID)
	if err != nil || node == nil {
		return c.actor
	}
	// TODO(等 AuthRealm 注册表): 这里要核 session.Realm 是站点配过的、且
	// realm.NodeType == node.Type（v2 就是那么做的）。现在会话只能由站点自己
	// CreateSession 建, 所以先不核。
	c.principal = node
	c.principalLoaded = true
	c.actor = Actor{
		NodeID:   node.ID,
		NodeType: node.Type,
		Realm:    session.Realm,
		Roles:    rolesOf(node),
	}
	return c.actor
}

// SetActor 装上一个已验证的身份 —— 插件与测试的入口（登录插件登录成功那一个请求内
// 直接用它, 免得再走一遍 cookie）。
func (c *CmsCtx) SetActor(actor Actor) {
	if !actor.IsAnonymous() && (actor.NodeType == "" || actor.Realm == "") {
		panic("web: incomplete node actor (node_type / realm required)")
	}
	if actor.IsAnonymous() {
		actor = Actor{}
	}
	c.actor = actor
	c.actorLoaded = true
	c.principal = nil
	c.principalLoaded = false
}

// Principal 身份背后的节点（匿名 ⇒ ErrNoPrincipal）。
func (c *CmsCtx) Principal() (*core.Node, error) {
	actor := c.Actor()
	if actor.IsAnonymous() {
		return nil, ErrNoPrincipal
	}
	if c.principalLoaded && c.principal != nil {
		return c.principal, nil
	}
	node, err := c.site.engine.GetNode(actor.NodeID)
	if err != nil {
		return nil, err
	}
	if node == nil || node.Type != actor.NodeType {
		return nil, ErrNoPrincipal
	}
	c.principal = node
	c.principalLoaded = true
	return node, nil
}

// authToken 令牌的来源（双轨）: Bearer 优先（API 客户端/脚本显式指定),
// 否则 cookie（浏览器）。
func (c *CmsCtx) authToken() string {
	header := c.R.Header.Get("Authorization")
	if header != "" {
		if token, ok := strings.CutPrefix(header, "Bearer "); ok {
			return strings.TrimSpace(token)
		}
		// 别的 scheme（Basic 等）不认 —— 当作没给
		if strings.Contains(header, " ") {
			return ""
		}
		// 裸令牌（有些客户端直接塞 Authorization）
		return strings.TrimSpace(header)
	}
	cookie, err := c.R.Cookie(TokenCookie)
	if err != nil || cookie == nil {
		return ""
	}
	return cookie.Value
}

// rolesOf 节点的角色: **类型名 + roles 字段的词表值**。
//
// roles 是能力注入的多选字段, 值在 fields JSON 里（字符串数组）; 用 cast 思路容错
// 取值而不是裸断言 —— 字段形态可能变（[]any / []string）。
func rolesOf(node *core.Node) []string {
	if node == nil {
		return nil
	}
	roles := make([]string, 0, 4)
	if node.Type != "" {
		roles = append(roles, node.Type)
	}
	switch list := node.Fields[types.RolesField].(type) {
	case []any:
		for _, value := range list {
			role, ok := value.(string)
			if ok && role != "" {
				roles = append(roles, role)
			}
		}
	case []string:
		roles = append(roles, list...)
	case string:
		if list != "" {
			roles = append(roles, list)
		}
	}
	return roles
}
