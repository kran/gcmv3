// 读入口（身份绑定层）。
//
// 两个层次别混:
//
//	CmsCtx.List / Get        客户端发起的读: 解析读规则（行范围 + 掩码）→ 调引擎 →
//	                         展开一层 → 套掩码。数据跨出进程前最后一步由这里保证。
//	engine.GetNode / GetNodes / CountNodes / ExpandNodes
//	                         内核原语: 没有身份、没有策略。后台、插件、迁移以及
//	                         "系统自己要看"的代码走这里 —— 那是显式的可信调用。
//
// 行范围由服务端算, 客户端条件**只能收窄**（AND 组合成子项）。core 那边的 Where 零值
// 是"不过滤", 所以这里保证**永远不会**把零值传下去: 策略没给范围就是配置错误（D1 的
// 求值会报错）, 客户端没给条件就用策略范围本身。
package web

import (
	"errors"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
)

// Get 单节点读: ref 是 int/int64（id）或 string（数字先当 id、否则当地址）。
//
// 读规则（行范围）→ 展开一层 → 字段掩码都做完。**不存在、不可见、类型不符都返回
// (nil, nil)** —— 调用方分不出"没有"和"看不到", 这正是要的（否则读接口变成存在性
// 探测器）; 回 404 还是回空页由调用方定。
//
// typeName 是**路由上的类型**（不是"从节点里读出来的"）: 地址是全表唯一的, 拿
// 别的类型的路由去打一个 id 也不该拿到东西。
func (c *CmsCtx) Get(typeName string, ref any) (*core.Node, error) {
	node, err := c.site.engine.GetNode(ref)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil, nil
		}
		return nil, CoreError(err)
	}
	if node == nil || node.Type != typeName {
		return nil, nil
	}
	visible, err := c.visible(node)
	if err != nil || !visible {
		return nil, err
	}
	rawFields := node.Fields // 掩码会把 Fields 换成新 map; 规则求值要用真值
	err = c.expandAndMask([]*core.Node{node})
	if err != nil {
		return nil, err
	}
	return c.withFacts(node, rawFields, nil), nil
}

// List 按读规则取一页（limit 0 = 不限）。返回节点与**不受 limit 限制**的总数
// （受内核统计上限约束: 超过 core.DefaultCountLimit 时 total 是个下限）。
//
// q 带上调用方要的条件与排序（q.Type 必填）; 条件是客户端的东西 ⇒ 只会与策略范围
// AND —— 传什么都放宽不了范围。排序也在 core 里按**声明**校验（kind 的 Sortable）。
//
// 展开一层（该类型的所有引用字段）后按类型掩码: 引用不会变成掩码的旁路。
func (c *CmsCtx) List(q core.NodeQuery, limit, offset int) ([]*core.Node, int64, error) {
	scope, _, err := c.readRule(q.Type)
	if err != nil {
		return nil, 0, err
	}
	if q.Where.IsZero() {
		q.Where = scope
	} else {
		q.Where = so.AND(q.Where, scope)
	}
	total, err := c.site.engine.CountNodes(q, 0)
	if err != nil {
		return nil, 0, CoreError(err)
	}
	// 计数不编译 ORDER BY ⇒ 空结果时短路会把"排序字段拼错"吞掉（200 + 空列表）。
	// 有排序就照样走一趟取列表, 让校验发生。
	if total == 0 && len(q.Sort) == 0 {
		return nil, 0, nil
	}
	nodes, err := c.site.engine.GetNodes(q, limit, offset)
	if err != nil {
		return nil, 0, CoreError(err)
	}
	err = c.expandAndMask(nodes)
	if err != nil {
		return nil, 0, err
	}
	// 列表不算 facts（逐项跑规则太贵）⇒ 给 `[]` 而不是 `null`: "没有可写字段"是
	// fail-closed 的那个答案（客户端据此不会把列表行画成可编辑）。
	for _, node := range nodes {
		node.Masked = nonNilStrings(node.Masked)
		node.Editable = nonNilStrings(node.Editable)
	}
	return nodes, total, nil
}

// visible 这个节点在当前身份的读范围里吗（用一次"最多数 1 条"的查询问存在性）。
func (c *CmsCtx) visible(node *core.Node) (bool, error) {
	if node == nil {
		return false, nil
	}
	scope, _, err := c.readRule(node.Type)
	if err != nil {
		return false, err
	}
	query := core.NodeQuery{
		Type:  node.Type,
		Where: so.AND(scope, so.P("=", "id", node.ID)),
	}
	count, err := c.site.engine.CountNodes(query, 1)
	if err != nil {
		return false, CoreError(err)
	}
	return count > 0, nil
}

// expandAndMask 就地展开一层再掩码。展开路径用 `*` = "该类型的所有引用字段"
// （core 按声明算路径, web 不重复一份字段知识）。
func (c *CmsCtx) expandAndMask(nodes []*core.Node) error {
	if len(nodes) == 0 {
		return nil
	}
	_, err := c.site.engine.ExpandNodes(nodes, "*")
	if err != nil {
		return CoreError(err)
	}
	return c.MaskNodes(nodes)
}
