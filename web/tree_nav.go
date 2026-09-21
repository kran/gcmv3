// 导航树（一层节点拼成树 + 导航操作）。
//
//	v2 的 core.Tree 承担这件事, 但那让内核背了"站点导航"这个概念。v3 的切法:
//	**读**由调用方走读入口（ctx.List —— 读规则/掩码照旧, 读不到的节点不会进树）,
//	这里只做**纯函数**: 吃一层节点 → 给一棵树 + 几个导航操作用法。
//
// 站点的模板函数因此只剩一行:
//
//	render.Func("tree", func(ctx *web.CmsCtx, typ string) (any, error) {
//	    nodes, _, err := ctx.List(core.NodeQuery{Type: typ}, 0, 0)
//	    return web.BuildTree(nodes, "parent"), err
//	})
//
// 模板侧（viicn 两站实测用到的就这些）:
//
//	{{ $t := tree "category" }}
//	{{ $t.Children "cases" }}      直接子级（tab 菜单）
//	{{ $t.Ancestors "media" }}     祖先链（面包屑 / 层级高亮）
//	{{ nodes $t $t.SubtreeIDs ... }} 子树下的内容
package web

import (
	"github.com/kran/gcmv3/core"
	"github.com/spf13/cast"
)

// Tree 一层节点拼成的导航树。
type Tree struct {
	nodes     []*core.Node
	byID      map[int64]*core.Node
	byAddress map[string]*core.Node
	children  map[int64][]*core.Node
	roots     []*core.Node
	parent    map[int64]int64
}

// BuildTree 拼树。nodes 必须**已经过读入口**（读不到的不会进来 —— 树不含策略）,
// parentField 是父引用字段名（如 category 的 "parent"）。
//
// 三种"当根"的情形: 没有父、父不在这一层里（不可见/未发布）、自指 —— 都不丢节点。
func BuildTree(nodes []*core.Node, parentField string) *Tree {
	out := &Tree{
		nodes:     nodes,
		byID:      make(map[int64]*core.Node, len(nodes)),
		byAddress: map[string]*core.Node{},
		children:  map[int64][]*core.Node{},
		parent:    map[int64]int64{},
	}
	for _, node := range nodes {
		out.byID[node.ID] = node
		if node.Address != nil && *node.Address != "" {
			out.byAddress[*node.Address] = node
		}
	}
	for _, node := range nodes {
		parentID := refID(node.Fields[parentField])
		_, parentVisible := out.byID[parentID]
		if parentID == 0 || !parentVisible || parentID == node.ID {
			if parentID != 0 {
				out.parent[node.ID] = parentID // 父不可见时记着（Ancestors 用不到, 但便于排查）
			}
			out.roots = append(out.roots, node)
			continue
		}
		out.parent[node.ID] = parentID
		out.children[parentID] = append(out.children[parentID], node)
	}
	return out
}

// Len 树里有多少节点。
func (t *Tree) Len() int {
	if t == nil {
		return 0
	}
	return len(t.nodes)
}

// Roots 根节点（保持读入顺序）。
func (t *Tree) Roots() []*core.Node {
	if t == nil {
		return nil
	}
	return t.roots
}

// Get 按 id **或地址**取节点（取不到返回 nil）。
//
// 地址也支持是因为**模板就是这么用的**: {{ $t.Children "news" }} —— 站点模板里
// 导航传的是地址（地址是稳定的 URL 段, 而 id 是库内部的）。
func (t *Tree) Get(ref any) *core.Node {
	if t == nil {
		return nil
	}
	return t.byID[t.resolve(ref)]
}

// resolve ref（id 或地址）→ id（取不到 = 0）。
func (t *Tree) resolve(ref any) int64 {
	if text, ok := ref.(string); ok {
		if text != "" && t.byAddress[text] != nil {
			return t.byAddress[text].ID
		}
	}
	return refID(ref)
}

// Children 直接子级（ref 取不到 ⇒ nil; 传 0/nil 时给根）。
func (t *Tree) Children(ref any) []*core.Node {
	if t == nil {
		return nil
	}
	id := t.resolve(ref)
	if id == 0 {
		return t.roots
	}
	return t.children[id]
}

// Parent 直接父节点（没有/父不可见 ⇒ nil）。
func (t *Tree) Parent(ref any) *core.Node {
	if t == nil {
		return nil
	}
	id := t.resolve(ref)
	if id == 0 {
		return nil
	}
	return t.byID[t.parent[id]]
}

// Ancestors 祖先链: **根 → …… → 父**（不含自己）—— 面包屑按顺序 range 即可。
func (t *Tree) Ancestors(ref any) []*core.Node {
	if t == nil {
		return nil
	}
	id := t.resolve(ref)
	if id == 0 {
		return nil
	}
	chain := []*core.Node{}
	// 先沿 parent 上溯, 再反转（自指/成环时靠 seen 截断, 不转死循环）
	seen := map[int64]bool{id: true}
	for current := t.parent[id]; current != 0 && !seen[current]; current = t.parent[current] {
		seen[current] = true
		node, ok := t.byID[current]
		if !ok {
			break
		}
		chain = append(chain, node)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// Subtree 子树（含自己, 前序）—— "这个分类下的全部内容"用它取 id 集合。
func (t *Tree) Subtree(ref any) []*core.Node {
	if t == nil {
		return nil
	}
	id := t.resolve(ref)
	root, ok := t.byID[id]
	if !ok {
		return nil
	}
	out := []*core.Node{root}
	seen := map[int64]bool{id: true}
	var walk func(int64)
	walk = func(parentID int64) {
		for _, child := range t.children[parentID] {
			if seen[child.ID] {
				continue
			}
			seen[child.ID] = true
			out = append(out, child)
			walk(child.ID)
		}
	}
	walk(id)
	return out
}

// SubtreeIDs 子树 id 列表（含自己）—— 拿去当查询条件（`in` 那个引用字段）。
func (t *Tree) SubtreeIDs(ref any) []int64 {
	nodes := t.Subtree(ref)
	if len(nodes) == 0 {
		return nil
	}
	ids := make([]int64, len(nodes))
	for i, node := range nodes {
		ids[i] = node.ID
	}
	return ids
}

// refID 取引用字段的 id（读投影里是 int64; 容错一点, 别裸断言 —— 字段类型多变）。
func refID(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	case string:
		return cast.ToInt64(typed)
	}
	return 0
}
