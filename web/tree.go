// 后台树视图 —— 一次把整棵树装好给前端（`admin.view: tree` 的类型）。
//
//	GET /admin/tree/{type}?sort=字段,-字段     sort 同 /api/nodes, 决定同级顺序
//
// 为什么不让前端拿列表自己拼: `/api/nodes` 的 size 上限是 100, 一棵几百个分类的树会被
// **悄悄截断** —— 少几行、还不报错, 是最难查的那种 bug。
//
// 三条:
//
//	① 走**受管读入口**（ctx.List）⇒ 读规则照旧生效: 读不到的行不进树, 掩码照旧
//	② 上限只是防呆（2000 个节点）; 超了**明确报错**, 绝不截断
//	③ 环 ⇒ 报错（带出环的链路）: 环里的节点既不是根、也不是任何可达节点的子节点,
//	   静默处理的结果就是"整片消失"。根因是树父字段没声明 `transitive: true`
//	   （引擎在写边时就不许成环）, 错误信息里会点出来。
package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/types"
)

// maxTreeNodes 树视图一次装的节点上限。
//
// 超过就报错（而不是截断）: 分类/地区这类层级表不该到几千; 真到了, 该换成分页列表
// 视图, 或者给这个类型加一层筛选。
const maxTreeNodes = 2000

// TreeNode 树里的一个节点: 节点的全部字段 + 子节点。
//
// 内嵌 core.Node ⇒ JSON 是平的（节点字段在上层, children 一起）—— 前端的
// el-table（row-key + tree-props.children）正是这么吃的。
type TreeNode struct {
	core.Node
	Children []*TreeNode `json:"children,omitempty"`
}

// adminTree GET /admin/tree/{type}
func (s *Site) adminTree(ctx *CmsCtx) {
	typeName := ctx.PathValue("type")
	def, ok := s.types.Type(typeName)
	if !ok {
		ctx.Fail(NotFound("类型 %q 不存在", typeName))
		return
	}
	if def.Admin.View != types.AdminViewTree {
		ctx.Fail(BadRequest("类型 %q 不是树视图（需要 types 里声明 admin.view: tree）", typeName))
		return
	}
	parentField := def.Admin.Tree
	if parentField == "" {
		// types 加载期就会拦下这种配置 —— 这里只是不信任"别处构造的 types"
		ctx.Fail(Internal("类型 %q 声明了 tree 视图但没给 admin.tree", typeName))
		return
	}
	sortFields, err := parseSort(ctx.Query("sort"))
	if err != nil {
		ctx.Fail(err)
		return
	}
	nodes, total, err := ctx.List(core.NodeQuery{Type: typeName, Sort: sortFields}, maxTreeNodes+1, 0)
	if err != nil {
		ctx.Fail(err)
		return
	}
	if len(nodes) > maxTreeNodes {
		ctx.Fail(Errorf(http.StatusUnprocessableEntity,
			"类型 %q 的节点太多（超过 %d）, 树视图一次画不完 —— 换列表视图, 或给类型加筛选",
			typeName, maxTreeNodes))
		return
	}
	roots, err := buildTree(nodes, parentField)
	if err != nil {
		ctx.Fail(BadRequest("%s", err.Error()))
		return
	}
	_ = ctx.Json(http.StatusOK, map[string]any{"items": roots, "total": total})
}

// buildTree 把平铺的节点按 parent 字段装成森林。
//
//	nodes  已按 sort 排好 ⇒ 同级顺序就是列表顺序（不再排一遍）
//	parent 不在结果集里的节点（父节点读不到/已被删）当作根 —— 不静默丢节点
//	不成环才返回; 成环报错并打印链路（环里的节点从根出发根本到不了）
func buildTree(nodes []*core.Node, parentField string) ([]*TreeNode, error) {
	byID := make(map[int64]*TreeNode, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = &TreeNode{Node: *node}
	}
	var roots []*TreeNode
	for _, node := range nodes {
		tree := byID[node.ID]
		parentID := parentOf(node, parentField)
		if parentID == 0 || parentID == node.ID {
			roots = append(roots, tree)
			continue
		}
		parent, ok := byID[parentID]
		if !ok {
			roots = append(roots, tree) // 父节点不在结果集里（读不到/已删）⇒ 当根
			continue
		}
		parent.Children = append(parent.Children, tree)
	}
	// 环检测: 从每个节点往上走, 走不到根 ⇒ 在环里
	for _, node := range nodes {
		if _, err := walkUp(byID, node, parentField); err != nil {
			return nil, err
		}
	}
	return roots, nil
}

// walkUp 顺着 parent 走到根, 顺路检测环。
func walkUp(byID map[int64]*TreeNode, node *core.Node, parentField string) ([]int64, error) {
	path := []int64{node.ID}
	seen := map[int64]bool{node.ID: true}
	for current := node; ; {
		parentID := parentOf(current, parentField)
		if parentID == 0 || parentID == current.ID {
			return path, nil // 到根了
		}
		parent, ok := byID[parentID]
		if !ok {
			return path, nil // 父节点不在结果集里 ⇒ 链断了, 也算到根
		}
		if seen[parentID] {
			return nil, fmt.Errorf("类型 %q 的 %s 字段里有环（%s）—— 给该字段声明 transitive: true, 引擎就不许写成环",
				node.Type, parentField, chain(path, parentID))
		}
		seen[parentID] = true
		path = append(path, parentID)
		current = &parent.Node
	}
}

func chain(path []int64, repeat int64) string {
	parts := make([]string, 0, len(path)+1)
	for _, id := range path {
		parts = append(parts, "#"+strconv.FormatInt(id, 10))
	}
	parts = append(parts, "#"+strconv.FormatInt(repeat, 10))
	return strings.Join(parts, " → ")
}

// parentOf 取节点的父 id（ref 字段在读投影里是 int64; 兼容别的数值形态）。
func parentOf(node *core.Node, parentField string) int64 {
	switch value := node.Fields[parentField].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			return 0
		}
		return parsed
	}
	return 0
}
