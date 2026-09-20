// 反向引用列表（admin.inbounds 的读端点）。
//
// 后台编辑器里"分类底下有哪些文章"这种东西: 声明写在类型上
// （category.admin.inbounds: [article.category]）, 前端拿这个端点现查。
//
// 三条设计:
//
//  1. **走受管读入口**（ctx.List）—— 读规则、掩码、展开全部照旧生效:
//     藏起来的节点不会出现在这个列表里（它不是掩码的旁路）。
//  2. **只读**: 入边的所有权在对面（是文章在引用分类）, 这里不给增删 ——
//     要改就点开那个节点改它的字段。
//  3. 现查而不是随节点下发: 一个分类可能几百篇, 每次打开编辑器都塞进响应里太贵。
package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
)

// inboundLimit 每个反向关系一次给多少（够看一眼; 更多的用"去列表页筛"）。
const inboundLimit = 20

// adminInbounds GET /admin/inbounds/{type}/{id}
//
// 响应: {items: [{spec, type, label, total, nodes: [...]}]} —— 按类型声明的顺序。
func (s *Site) adminInbounds(ctx *CmsCtx) {
	typeName := strings.TrimSpace(ctx.PathValue("type"))
	nodeID, err := strconv.ParseInt(strings.TrimSpace(ctx.PathValue("id")), 10, 64)
	if err != nil || nodeID <= 0 {
		ctx.Fail(BadRequest("id 必须是正整数"))
		return
	}
	if _, ok := s.types.Type(typeName); !ok {
		ctx.Fail(NotFound("类型不存在"))
		return
	}
	// 只对**存在且读得到**的节点给列表（读不到 = 当它不存在, 不暴露存在性）
	node, err := ctx.Get(typeName, nodeID)
	if err != nil {
		ctx.Fail(CoreError(err))
		return
	}
	if node == nil {
		ctx.Fail(NotFound("节点不存在"))
		return
	}

	declared := s.types.Inbounds(typeName)
	items := make([]map[string]any, 0, len(declared))
	for _, in := range declared {
		// 引用方类型里有字段指向我 ⇒ 条件就是那个引用字段里含我。
		// 注意用 **in**（单元素集合）而不是 `=`: 引用路径只收 in/exists/ref
		//（编译器当场报 "->x needs in/exists/ref, not ="）。
		nodes, total, err := ctx.List(core.NodeQuery{
			Type:  in.Type,
			Where: so.P("in", "->"+in.Field, []int64{nodeID}),
		}, inboundLimit, 0)
		if err != nil {
			ctx.Fail(CoreError(err))
			return
		}
		items = append(items, map[string]any{
			"spec": in.Spec, "type": in.Type, "field": in.Field, "label": in.Label,
			"total": total, "nodes": nodes,
		})
	}
	_ = ctx.Json(http.StatusOK, map[string]any{"items": items})
}
