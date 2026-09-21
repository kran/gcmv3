package web

import (
	"testing"

	"github.com/kran/gcmv3/core"
)

// 树: 根/子级/祖先链/子树（含"父不可见当根""自指不成环"）。
func TestBuildTreeNavigation(t *testing.T) {
	node := func(id int64, name string, parent int64) *core.Node {
		fields := core.Fields{"name": name}
		if parent != 0 {
			fields["parent"] = parent
		}
		return &core.Node{ID: id, Type: "category", Fields: fields}
	}
	// 1 ─ 2 ─ 3 ；4 的父(9)不可见 ⇒ 当根；5 自指 ⇒ 当根
	nodes := []*core.Node{
		node(1, "根", 0), node(2, "子", 1), node(3, "孙", 2),
		node(4, "父不可见", 9), node(5, "自指", 5),
	}
	tree := BuildTree(nodes, "parent")

	if tree.Len() != 5 {
		t.Fatalf("不该丢节点: %d", tree.Len())
	}
	if len(tree.Roots()) != 3 {
		t.Fatalf("根该有 3 个（1 / 4 / 5, 顺序按读入）: %d", len(tree.Roots()))
	}
	if got := names(tree.Children(1)); len(got) != 1 || got[0] != "子" {
		t.Fatalf("Children(1) = %v", got)
	}
	if got := names(tree.Children(0)); len(got) != 3 {
		t.Fatalf("Children(0) 该给根: %v", got)
	}
	// 祖先链: 根 → 父（不含自己）
	if got := names(tree.Ancestors(3)); len(got) != 2 || got[0] != "根" || got[1] != "子" {
		t.Fatalf("Ancestors(3) = %v（该是 根→子）", got)
	}
	if got := tree.Ancestors(1); len(got) != 0 {
		t.Fatalf("根的祖先链该为空: %v", names(got))
	}
	// 子树: 含自己, 前序
	if got := tree.SubtreeIDs(1); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("SubtreeIDs(1) = %v", got)
	}
	if tree.Get(2) == nil || tree.Get(999) != nil {
		t.Fatal("Get 该按 id 命中/落空")
	}
	// 自指/成环不能转死: Ancestors(5) 立刻停
	if got := tree.Ancestors(5); len(got) != 0 {
		t.Fatalf("自指节点的祖先链该为空: %v", names(got))
	}
}

func names(nodes []*core.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, node.Fields.Str("name"))
	}
	return out
}

// 模板用**地址**在树里导航（真实模板: {{ $t.Children "news" }}）—— 只按 id 索引会静默
// 拿到根（字符串 cast 成 0），所以地址必须也能定位。
func TestTreeResolveByAddress(t *testing.T) {
	address := func(value string) *string { return &value }
	root := &core.Node{ID: 1, Type: "category", Address: address("news"),
		Fields: core.Fields{"name": "新闻"}}
	child := &core.Node{ID: 2, Type: "category", Address: address("news-industry"),
		Fields: core.Fields{"name": "行业", "parent": int64(1)}}
	tree := BuildTree([]*core.Node{root, child}, "parent")

	if got := tree.Get("news"); got == nil || got.ID != 1 {
		t.Fatalf("按地址取节点: %#v", got)
	}
	if children := tree.Children("news"); len(children) != 1 || children[0].ID != 2 {
		t.Fatalf("按地址取子级: %#v", names(children))
	}
	if ancestors := tree.Ancestors("news-industry"); len(ancestors) != 1 || ancestors[0].ID != 1 {
		t.Fatalf("按地址取祖先链: %#v", names(ancestors))
	}
	if ids := tree.SubtreeIDs("news"); len(ids) != 2 {
		t.Fatalf("按地址取子树 id: %#v", ids)
	}
	// 不认识的地址 ⇒ 当"没有"（返回根/空, 不 panic）
	if got := tree.Children("nope"); len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("未知引用该退回根（只有 新闻 是根）: %#v", names(got))
	}
}
