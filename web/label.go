// 读期事实第三项: 节点的**显示名**（extra.display）。
//
// 名字来自**类型声明** admin.display —— 和 masked / editable 一样, 这是"声明 + 规则
// 推出来的读期事实", 由读层一次算好, 前端不用猜字段名（猜就有第二份真相）。
//
// 推导顺序（与后台 refLabel 同一套）:
//
//  1. admin.display 指向的字段
//  2. admin.columns 里第一个有值的
//  3. 声明字段里第一个有值的
//  4. "#<id>"
//
// 两条边界:
//
//	只从**掩码之后**的字段里找 —— 显示名字段本身不可读时顺着往下走, 绝不能从
//	标签把被裁掉的值漏出去（标签是"顺手给的便利", 不能变成掩码的旁路）。
//
//	只填 Extra（"渲染期附加数据: 不落库"）—— 不给 core.Node 加顶层 display:
//	顶层 display 是 v3 去掉的那个"幽灵字段"（谁都能塞、与声明无关）, 这里给的是
//	**由声明决定的**显示名。
package web

import (
	"strconv"
	"strings"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/types"
)

// displayKey Extra 里放显示名的键（前端读 node.extra.display）。
const displayKey = "display"

// labelNode 给自己 + 展开出去的子节点（含更深层）补显示名。
//
// 由 MaskNode 调用（节点跨出进程的唯一关口）—— 展开目标也要: 前台拿 expand 里的
// 引用渲染"行业/地区/作者/分类"这类标签, 那些节点不在顶层列表里, 不补就没名字。
func (c *CmsCtx) labelNode(node *core.Node) {
	if node == nil {
		return
	}
	def, ok := c.site.types.Type(node.Type)
	if !ok {
		return // 类型不认识（不该发生）: 不编造事实
	}
	label := displayValue(def, node.Fields)
	if label == "" {
		label = "#" + strconv.FormatInt(node.ID, 10)
	}
	if node.Extra == nil {
		node.Extra = map[string]any{}
	}
	node.Extra[displayKey] = label
	c.labelExpand(node.Expand)
}

// labelExpand 展开容器里的子节点（core 那边存 *core.Node 或 []*core.Node）。
func (c *CmsCtx) labelExpand(expand map[string]any) {
	for _, value := range expand {
		switch typed := value.(type) {
		case *core.Node:
			c.labelNode(typed)
		case []*core.Node:
			for _, child := range typed {
				c.labelNode(child)
			}
		}
	}
}

// displayValue 按声明挑显示名。fields 必须是**掩码后**的那份。
func displayValue(def types.TypeDef, fields core.Fields) string {
	if def.Admin.Display != "" {
		if value := strings.TrimSpace(fields.Str(def.Admin.Display)); value != "" {
			return value
		}
	}
	for _, name := range def.Admin.Columns {
		if value := strings.TrimSpace(fields.Str(name)); value != "" {
			return value
		}
	}
	for _, field := range def.Fields {
		if value := strings.TrimSpace(fields.Str(field.Name)); value != "" {
			return value
		}
	}
	return ""
}
