// 响应里的**节点事实** —— 前端（尤其是后台表单）靠它渲染:
//
//	masked    这次响应里被读规则裁掉的字段（"看起来空"不等于"没值"）
//	editable  这个节点上、按这次提交的 patch, 当前身份实际能写哪些字段
//
// 两个事实都填在 **core.Node 的 `db:"-"` 字段**上（core 只声明、从不读它们; 谁填由
// web 决定）。只在**单节点**响应里算 —— 列表要逐项跑一遍规则, 那是笔冤枉账。
//
// 不变式: editable ∩ masked = ∅（可写 ⊆ 可读, D1 定的）—— 界面不会把"看不见的字段"
// 摆成可编辑的。
//
// 前端的用法（约定）:
//
//	masked 里的字段: 显示"无权限查看"这类占位, 不要显示值
//	editable 里没有的字段: 显示值, 但**不可编辑**（只读）
//
// 两个事实在任何响应里都是**数组**: `[]` 是"没隐藏 / 一个都不能写"，不是缺席。
// 列表接口不算它们（逐项跑规则太贵），那里给的是 `[]` —— **别拿列表行判断可写**,
// 打开表单时要读一次单节点详情。
package web

import (
	"maps"
	"slices"

	"github.com/kran/gcmv3/core"
)

// withFacts 给响应里的节点补两个事实（就地改 masked, 返回它）。
//
// rawFields 是**掩码前**的字段: 规则求值要看到真值 —— 规则不受掩码约束（它就是决定
// 掩码的那个人）, 让隐藏字段悄悄改变"能不能写"的判断是 bug, 不是安全。
//
// patch 是这次提交的差量（GET 传 nil）: editable 拿它去问更新规则 —— 规则可能要看
// patch 里改了什么才能回答"这个能不能写"。固有近似: patch 为空时只能算出"一般情况下
// 能写什么"; 逐字段互相依赖的规则只能在真提交时由规则自己判。
func (c *CmsCtx) withFacts(masked *core.Node, rawFields core.Fields, patch *core.NodePatch) *core.Node {
	if masked == nil {
		return nil
	}
	_, hidden, err := c.readRule(masked.Type)
	if err != nil {
		// 读规则报错时走不到这里（读入口已经先报了）; 保险起见不编造事实
		return masked
	}
	// 两个事实一律**非 nil**: 空集合要下发成 `[]`，不是 `null`/缺席 ——
	// 客户端把"缺 editable"读成"没有限制"就是 fail-open（真实踩过: 员工表单里
	// "角色"是勾选框, 而那个类型根本没有写规则）。
	masked.Masked = nonNilStrings(hidden)
	masked.Editable = nonNilStrings(c.editableFields(masked, rawFields, patch, hidden))
	return masked
}

// nonNilStrings nil → 空切片（"没有"与"没算"在 JSON 里都得看得见）。
func nonNilStrings(names []string) []string {
	if names == nil {
		return []string{}
	}
	return names
}

// editableFields 求"这个节点上能写哪些字段"。
//
// 做法是**真的跑一遍更新规则**（拿一个丢弃用的 Grant）, 再按声明顺序取出被授予的字段 ——
// 不写第二个真相来源（矩阵接口用的是同一招）。
//
// 副作用说明: 规则求值可能有副作用（读库没问题; 真去写库的规则就是不守规矩）,
// 所以只在单节点响应里做。
func (c *CmsCtx) editableFields(node *core.Node, rawFields core.Fields, patch *core.NodePatch, hidden []string) []string {
	policy := c.site.policy(node.Type)
	if policy == nil || policy.OnUpdate == nil {
		return nil
	}
	// 拷贝 patch: 规则可能就地加工它（补默认值/删字段）, 别污染调用方的那份
	probe := core.NodePatch{}
	if patch != nil {
		probe = *patch
		if len(patch.Fields) > 0 {
			probe.Fields = make(core.Fields, len(patch.Fields))
			maps.Copy(probe.Fields, patch.Fields)
		}
	}
	// 给规则一个"同一节点 + 未掩码字段"的视图（不借出我们自己那份）
	probeNode := *node
	probeNode.Fields = rawFields
	allow := newWriteGrant()
	err := policy.OnUpdate(c, &probeNode, &probe, allow)
	if err != nil {
		// 这次提交不允许更新 ⇒ 没有可写字段（不是"全都是"）
		return nil
	}
	def, ok := c.site.types.Type(node.Type)
	if !ok {
		return nil
	}
	roles := c.Actor().Roles
	out := make([]string, 0, len(def.Fields))
	for _, field := range def.Fields {
		if slices.Contains(hidden, field.Name) {
			continue // 不该发生（可写 ⊆ 可读）; 真出现了也不让它进可写集合
		}
		if allow.Has(roles, field.Name) {
			out = append(out, field.Name)
		}
	}
	return out
}
