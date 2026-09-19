package core

import (
	"errors"
	"strings"
	"testing"
)

// 出边展开: 字段形状按 kind 决定（ref → 单个 *Node, refs → []*Node）。
func TestExpandOutRefs(t *testing.T) {
	gcm, news, tech, a1, a2, _ := seed(t)

	node, err := gcm.GetNode(a2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gcm.ExpandNode(node, "categories"); err != nil {
		t.Fatal(err)
	}
	cats, ok := node.Expand["categories"].([]*Node)
	if !ok || len(cats) != 2 {
		t.Fatalf("refs 该展开成 []*Node: %#v", node.Expand["categories"])
	}
	if cats[0].ID != news || cats[1].ID != tech {
		t.Fatalf("边序 = %d,%d", cats[0].ID, cats[1].ID)
	}
	// 目标节点的 Fields 完整（引用 id 也在里面）
	if cats[0].Fields.Str("name") != "新闻" {
		t.Fatalf("目标字段 = %#v", cats[0].Fields)
	}
	_ = a1
}

// 单引用字段展开成 **单个** *Node（不是切片）。
func TestExpandSingleRef(t *testing.T) {
	gcm := openFixture(t)
	a, err := gcm.CreateNode(nil, &Node{Type: "person", Fields: Fields{"name": "A"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := gcm.CreateNode(nil, &Node{Type: "person", Fields: Fields{"name": "B", "mentor": a}})
	if err != nil {
		t.Fatal(err)
	}
	node, err := gcm.GetNode(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gcm.ExpandNode(node, "mentor"); err != nil {
		t.Fatal(err)
	}
	mentor, ok := node.Expand["mentor"].(*Node)
	if !ok || mentor.ID != a {
		t.Fatalf("ref 该展开成 *Node: %#v", node.Expand["mentor"])
	}
}

// 一条路径多段: 链式推进（然后对目标集继续展开下一段）。
func TestExpandChain(t *testing.T) {
	gcm := openFixture(t)
	root, _ := gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "root"}})
	child, _ := gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "child", "parent": root}})
	art, err := gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{"title": "文", "categories": []any{child}}})
	if err != nil {
		t.Fatal(err)
	}
	node, err := gcm.GetNode(art)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gcm.ExpandNode(node, "categories.parent"); err != nil {
		t.Fatal(err)
	}
	cats, ok := node.Expand["categories"].([]*Node)
	if !ok || len(cats) != 1 {
		t.Fatalf("第一段 = %#v", node.Expand["categories"])
	}
	// 第二段挂在**目标节点**上（不是根节点）
	parent, ok := cats[0].Expand["parent"].(*Node)
	if !ok || parent.ID != root {
		t.Fatalf("第二段 = %#v", cats[0].Expand)
	}
}

// 入边展开: <-type.field —— 来源类型必须显式。
func TestExpandIncoming(t *testing.T) {
	gcm, news, _, a1, a2, _ := seed(t)
	node, err := gcm.GetNode(news)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gcm.ExpandNode(node, "<-article.categories"); err != nil {
		t.Fatal(err)
	}
	arts, ok := node.Expand["<-article.categories"].([]*Node)
	if !ok || len(arts) != 2 {
		t.Fatalf("入边 = %#v", node.Expand["<-article.categories"])
	}
	got := map[int64]bool{arts[0].ID: true, arts[1].ID: true}
	if !got[a1] || !got[a2] {
		t.Fatalf("入边目标 = %#v", got)
	}
}

// 一批节点: 同类型一次展开（SQL 次数与节点数无关 —— 这里只验行为, 次数由
// captureQueries 那种手段才看得出, 但至少要保证批量不乱）。
func TestExpandBatch(t *testing.T) {
	gcm, _, _, a1, a2, a3 := seed(t)
	nodes, err := gcm.GetNodesByIDs([]int64{a1, a2, a3})
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := gcm.ExpandNodes(nodes, "categories")
	if err != nil {
		t.Fatal(err)
	}
	if len(expanded) != 3 {
		t.Fatalf("返回 = %d", len(expanded))
	}
	byID := map[int64]*Node{}
	for _, n := range expanded {
		byID[n.ID] = n
	}
	if len(byID[a1].Expand["categories"].([]*Node)) != 1 {
		t.Fatalf("a1 = %#v", byID[a1].Expand["categories"])
	}
	if len(byID[a2].Expand["categories"].([]*Node)) != 2 {
		t.Fatalf("a2 = %#v", byID[a2].Expand["categories"])
	}
	// 没有引用的节点拿到空切片（键在, 值为空）—— 前端不必判 nil
	if values, ok := byID[a3].Expand["categories"].([]*Node); !ok || len(values) != 0 {
		t.Fatalf("a3 = %#v", byID[a3].Expand["categories"])
	}
}

// 一批里混了多个类型: 路径必须**对每个类型都成立** —— 否则 fail-loud。
//
// 为什么不错过就算（静默跳过）: 路径是按**类型**给的（后台 expand 参数、模板
// 都针对一个类型）⇒ 不适用的字段名是调用方写错了, 不能什么都不做。
func TestExpandMixedTypesFailsLoud(t *testing.T) {
	gcm, news, _, a1, _, _ := seed(t)
	art, err := gcm.GetNode(a1)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := gcm.GetNode(news)
	if err != nil {
		t.Fatal(err)
	}
	// category 没有 categories 字段
	_, err = gcm.ExpandNodes([]*Node{art, cat}, "categories")
	if !errors.Is(err, ErrInvalidField) {
		t.Fatalf("err = %v, want ErrInvalidField", err)
	}
	if !strings.Contains(err.Error(), "category") {
		t.Fatalf("错误要说清是哪个类型: %v", err)
	}
}

// 展开出来的目标节点，Fields 也必须完整（引用 id 要投影回来）——
// 它们和别的读出口一样是"读出来的节点"。
func TestExpandTargetFieldsComplete(t *testing.T) {
	gcm := openFixture(t)
	root, _ := gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "root"}})
	child, _ := gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "child", "parent": root}})
	art, err := gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{"title": "文", "categories": []any{child}}})
	if err != nil {
		t.Fatal(err)
	}
	node, err := gcm.GetNode(art)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gcm.ExpandNode(node, "categories"); err != nil {
		t.Fatal(err)
	}
	target := node.Expand["categories"].([]*Node)[0]
	// child 自己有 parent 引用 —— 必须被投影出来, 否则"读→写"往返就把它丢了
	parents, ok := target.Fields["parent"].(int64)
	if !ok || parents != root {
		t.Fatalf("目标的引用字段没投影: %#v", target.Fields)
	}
	// 空的多引用字段也要是 []int64（与读出口一致）
	if authors, ok := target.Fields["authors"].([]int64); ok && len(authors) != 0 {
		t.Fatalf("不该有 authors: %#v", authors)
	}
}

// `*` = 该类型的所有引用字段（单层）, 键还是字段名。
func TestExpandAll(t *testing.T) {
	gcm := openFixture(t)
	person, _ := gcm.CreateNode(nil, &Node{Type: "person", Fields: Fields{"name": "作者"}})
	cat, _ := gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "分类"}})
	art, err := gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{
		"title": "文", "categories": []any{cat}, "authors": []any{person},
	}})
	if err != nil {
		t.Fatal(err)
	}
	node, err := gcm.GetNode(art)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gcm.ExpandNode(node, "*"); err != nil {
		t.Fatal(err)
	}
	if _, ok := node.Expand["categories"]; !ok {
		t.Fatalf("categories 没展开: %#v", node.Expand)
	}
	if _, ok := node.Expand["authors"]; !ok {
		t.Fatalf("authors 没展开: %#v", node.Expand)
	}
	// 没有 "*" 这个键
	if _, ok := node.Expand["*"]; ok {
		t.Fatalf("键该是字段名: %#v", node.Expand)
	}
	// 非引用字段不展开
	if _, ok := node.Expand["title"]; ok {
		t.Fatalf("title 不是引用: %#v", node.Expand)
	}
	// 没有引用字段的类型: 空操作（不报错）
	catNode, err := gcm.GetNode(cat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gcm.ExpandNode(catNode, "*"); err != nil {
		t.Fatalf("category 只有 parent（引用）—— 该展开它, 不该报错: %v", err)
	}
}

// paths 为空 = 不展开（不是"按类型自动展开"）。
func TestExpandNoPaths(t *testing.T) {
	gcm, _, _, a1, _, _ := seed(t)
	node, err := gcm.GetNode(a1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gcm.ExpandNode(node); err != nil {
		t.Fatal(err)
	}
	if node.Expand != nil {
		t.Fatalf("没给路径不该展开: %#v", node.Expand)
	}
}

// 路径解析: 形态错误、深度上限、路径数上限。
func TestExpandPathValidation(t *testing.T) {
	gcm, _, _, a1, _, _ := seed(t)
	node, err := gcm.GetNode(a1)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
		want error
	}{
		{"空路径", "", ErrInvalidQuery},
		{"入边缺字段", "<-article", ErrInvalidQuery},
		{"入边缺来源类型", "<-.categories", ErrInvalidQuery},
		{"字段名空", "categories..parent", ErrInvalidQuery},
		{"字段不存在", "ghost", ErrInvalidField},
		{"不是引用字段", "title", ErrInvalidField},
		{"入边目标不对", "<-person.name", ErrInvalidField},
		{"深度超限", "categories.parent.parent.parent.parent", ErrQueryTooComplex},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := gcm.ExpandNode(node, test.path)
			if !errors.Is(err, test.want) {
				t.Fatalf("err = %v, want %v", err, test.want)
			}
		})
	}

	many := make([]string, maxExpandPaths+1)
	for i := range many {
		many[i] = "categories"
	}
	if _, err := gcm.ExpandNode(node, many...); !errors.Is(err, ErrQueryTooComplex) {
		t.Fatalf("路径数上限: %v", err)
	}
}

// nil 节点要报错（不是 panic）。
func TestExpandNilNode(t *testing.T) {
	gcm := openFixture(t)
	if _, err := gcm.ExpandNode(nil, "categories"); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("err = %v", err)
	}
}

// 边数上限: 超了报错, 不静默截断。
func TestExpandEdgeLimit(t *testing.T) {
	gcm := openFixture(t)
	cat, err := gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "分类"}})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]any, 0, maxExpandEdges+1)
	for i := 0; i <= maxExpandEdges; i++ {
		ids = append(ids, cat)
	}
	// 直接写边（绕过应用层去重 —— 这是"库里已经有脏数据"的场景）
	for i := 0; i <= maxExpandEdges; i++ {
		target, err := gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{"title": "t"}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = gcm.db.Insert("edges", map[string]any{
			"from_node": target, "field": "categories", "to_node": cat,
			"sort": 0, "created_at": nowValue(),
		}).Exec()
		if err != nil {
			t.Fatal(err)
		}
	}
	node, err := gcm.GetNode(cat)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gcm.ExpandNode(node, "<-article.categories")
	if !errors.Is(err, ErrQueryTooComplex) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("错误要说清是超限: %v", err)
	}
}
