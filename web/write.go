// 写入口（身份绑定层）。
//
// 与读入口同样的两层分工:
//
//	CmsCtx.Create / Update / Delete   客户端发起的写: 过策略（身份 + 可写字段）
//	                                  → 落库 → 返回**掩码后**的节点。
//	engine.CreateNode / PatchNode / DeleteNode
//	                                  内核原语: 系统的写（审批、计数、导入、迁移、
//	                                  后台批处理）。那些调用没有"客户端授权"可言。
//
// **看得到才改得到**: Update / Delete 先按读范围确认这个节点当前身份看得见, 看不见
// 一律 404（与读同一个语义, 不区分"没有"和"看不到"）。这一步是结构性保证 —— 站点
// 忘在写规则里判归属, 也改不了范围外的行; v2 没有这一步, 靠每个站点的
// OnUpdate/OnDelete 自己记得判身份。
//
// 失败是 web.Error（core 的错误在这里就翻译完）—— 调用方不需要 errors.Is。
package web

import (
	"errors"

	"github.com/kran/gcmv3/core"
)

// Create 走创建规则写入, 返回掩码后的节点。
//
// fields 是**客户端提交的**字段（不要预先塞默认值: 默认值由引擎在策略之后应用,
// 否则它们会进白名单被误拒）。规则可以就地改节点（补 author、改状态）。
//
// 顺序: 授权（注册了规则吗）→ 落库 —— 创建没有"存在性"可泄漏。
func (c *CmsCtx) Create(typeName string, fields core.Fields) (*core.Node, error) {
	if fields == nil {
		fields = core.Fields{}
	}
	node := &core.Node{Type: typeName, Fields: fields}
	err := c.authorizeCreate(node)
	if err != nil {
		return nil, err
	}
	id, err := c.site.engine.CreateNode(c.DB(), node)
	if err != nil {
		return nil, CoreError(err)
	}
	return c.readBack(id, nil)
}

// Update 走更新规则写入, 返回掩码后的节点。
//
// patch.Revision 是**必填**的（乐观锁: 客户端读到几就写几）—— 缺了引擎会拒绝,
// 这里不替它补一个"读出来的最新版本"（那等于把乐观锁关掉）。
//
// 顺序（**授权先于存在性**）: ①这个类型注册了更新规则吗（否则匿名能靠 401/404 的
// 差别探测 id 存不存在）②节点存在 / 看得见 / 类型对得上 ③规则 ④落库。
func (c *CmsCtx) Update(typeName string, id int64, patch *core.NodePatch) (*core.Node, error) {
	err := c.requireRule(typeName, VerbUpdate)
	if err != nil {
		return nil, err
	}
	existing, err := c.writable(typeName, id)
	if err != nil {
		return nil, err
	}
	err = c.authorizeUpdate(existing, patch)
	if err != nil {
		return nil, err
	}
	err = c.site.engine.PatchNode(c.DB(), id, patch)
	if err != nil {
		return nil, CoreError(err)
	}
	// 事实用**这次提交的 patch** 算: 改完状态之后可写集合可能就变了
	return c.readBack(id, patch)
}

// Delete 走删除规则, 然后永久删除（被引用则拒绝）。"下线/撤回"不是内核概念 ——
// 用类型自己的状态字段表达, 并在读规则里限制范围。
//
// 顺序同 Update（授权先于存在性）。
func (c *CmsCtx) Delete(typeName string, id int64) error {
	err := c.requireRule(typeName, VerbDelete)
	if err != nil {
		return err
	}
	existing, err := c.writable(typeName, id)
	if err != nil {
		return err
	}
	err = c.authorizeDelete(existing)
	if err != nil {
		return err
	}
	err = c.site.engine.DeleteNode(c.DB(), id)
	if err != nil {
		return CoreError(err)
	}
	return nil
}

// writable 取"当前身份看得见的、且类型对得上"的节点 —— 写路径的第二道闸门。
//
// 不存在、看不见（不在读范围里）、类型不符都回 404（同一个语义, 客户端分不出来）。
func (c *CmsCtx) writable(typeName string, id int64) (*core.Node, error) {
	node, err := c.site.engine.GetNode(id)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil, NotFound("不存在")
		}
		return nil, CoreError(err)
	}
	if node == nil || node.Type != typeName {
		return nil, NotFound("不存在")
	}
	visible, err := c.visible(node)
	if err != nil {
		return nil, err
	}
	if !visible {
		return nil, NotFound("不存在")
	}
	return node, nil
}

// readBack 写响应里的节点也按读规则掩码 + 补两个事实 —— "注册了读规则 ⇒ 输出已裁"
// 对写响应同样成立（否则写接口就成了掩码的旁路）。
//
// 这里**不展开**: 写响应的意义是"你刚写的那个节点", 要看引用目标再读一次就行。
func (c *CmsCtx) readBack(id int64, patch *core.NodePatch) (*core.Node, error) {
	node, err := c.site.engine.GetNode(id)
	if err != nil {
		return nil, CoreError(err)
	}
	if node == nil {
		return nil, nil
	}
	rawFields := node.Fields // MaskNode 不改入参, 这里拿到的是真值
	masked, err := c.MaskNode(node)
	if err != nil {
		return nil, err
	}
	return c.withFacts(masked, rawFields, patch), nil
}
