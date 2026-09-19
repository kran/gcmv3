package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	gql "github.com/kran/gcmv3/so"
)

// seed 建几个节点: 2 个分类 + 3 篇文章（含引用与排序字段）。
func seed(t *testing.T) (*GCM, int64, int64, int64, int64, int64) {
	t.Helper()
	gcm := openFixture(t)
	news, err := gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "新闻", "address": "news"}})
	if err != nil {
		t.Fatal(err)
	}
	tech, err := gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "科技", "address": "tech"}})
	if err != nil {
		t.Fatal(err)
	}
	a1, err := gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{
		"title": "甲", "views": int64(10), "state": "published", "categories": []any{news},
	}})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{
		"title": "乙", "views": int64(30), "state": "draft", "categories": []any{news, tech},
	}})
	if err != nil {
		t.Fatal(err)
	}
	a3, err := gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{
		"title": "丙", "views": int64(20), "state": "published",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return gcm, news, tech, a1, a2, a3
}

// 读投影: 引用 id 必须回到 Fields 里（单引用是 int64, 多引用是 []int64）。
func TestGetNodeProjectsRefs(t *testing.T) {
	gcm, news, tech, a1, a2, a3 := seed(t)

	node, err := gcm.GetNode(a1)
	if err != nil {
		t.Fatal(err)
	}
	if node.Fields.Str("title") != "甲" || node.Fields.Int("views") != 10 {
		t.Fatalf("标量 = %#v", node.Fields)
	}
	// 单引用字段的投影
	if got, ok := node.Fields["mentor"]; ok && got != nil {
		t.Fatalf("article 没有 mentor: %#v", got)
	}
	cats, ok := node.Fields["categories"].([]int64)
	if !ok || len(cats) != 1 || cats[0] != news {
		t.Fatalf("多引用投影 = %#v", node.Fields["categories"])
	}

	// 多引用（两个）
	node2, err := gcm.GetNode(a2)
	if err != nil {
		t.Fatal(err)
	}
	cats2 := node2.Fields["categories"].([]int64)
	if len(cats2) != 2 || cats2[0] != news || cats2[1] != tech {
		t.Fatalf("边序 = %#v", cats2)
	}

	// 空的引用字段也必须是 []int64（读出来的值要能原样写回）
	node3, err := gcm.GetNode(a3)
	if err != nil {
		t.Fatal(err)
	}
	empty, ok := node3.Fields["categories"].([]int64)
	if !ok || len(empty) != 0 {
		t.Fatalf("空引用 = %#v", node3.Fields["categories"])
	}
}

// GetNode: 数字 → id; 字符串 → 地址。两条路都由**数据**决定, 不猜。
func TestGetNodeByIDOrAddress(t *testing.T) {
	gcm, news, _, a1, _, _ := seed(t)
	if _, err := gcm.GetNode(int64(a1)); err != nil {
		t.Fatal(err)
	}
	if _, err := gcm.GetNode(int(a1)); err != nil {
		t.Fatalf("int 也该认: %v", err)
	}
	// 地址（addressable 类型注入的字段）
	node, err := gcm.GetNode("news")
	if err != nil {
		t.Fatal(err)
	}
	if node.ID != news {
		t.Fatalf("按地址取到 %d, want %d", node.ID, news)
	}
	if node.Address == nil || *node.Address != "news" {
		t.Fatalf("生成列 = %#v", node.Address)
	}
	// 没这个地址 / 空地址 / 非整数
	if _, err := gcm.GetNode("不存在的地址"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := gcm.GetNode(""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空地址: %v", err)
	}
	if _, err := gcm.GetNode(1.5); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("非整数: %v", err)
	}
	if _, err := gcm.GetNode(int64(9999)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// 地址查询**必须走索引** —— 这是存储层用"生成列"而不是 JSON 表达式的全部理由
// （实测: 索引建在 json_extract 表达式上时按地址查是全表扫）。
func TestAddressLookupUsesIndex(t *testing.T) {
	gcm := openFixture(t)
	// 行数太小时 planner 一定选扫表, 说明不了问题
	for i := 0; i < 2000; i++ {
		_, err := gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{
			"title": "t", "address": fmt.Sprintf("a-%d", i),
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := gcm.db.Add(`ANALYZE`).Exec()
	if err != nil {
		t.Fatal(err)
	}

	rows, err := gcm.db.Pool().Query(`EXPLAIN QUERY PLAN SELECT * FROM nodes WHERE address = ?`, "a-7")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var a, b, c int
		var detail string
		rows.Scan(&a, &b, &c, &detail)
		plan.WriteString(detail + " | ")
	}
	if !strings.Contains(plan.String(), "USING") || !strings.Contains(plan.String(), "INDEX") {
		t.Fatalf("地址查询没走索引: %s", plan.String())
	}
	if _, err := gcm.GetNode("a-7"); err != nil {
		t.Fatal(err)
	}
}

// 地址既是**字段**（在 fields 里, 走 JSON 路径）也是**系统列**（生成列, 走索引）。
// 两条路语义相同 —— 查询/排序用系统列, 授权/表单/投影用字段。
func TestAddressIsFieldAndColumn(t *testing.T) {
	gcm, news, _, _, _, _ := seed(t)
	byColumn, err := gcm.GetNodes(NodeQuery{Type: "category", Where: gql.P("=", "address", "news")}, 0, 0)
	if err != nil || len(byColumn) != 1 || byColumn[0].ID != news {
		t.Fatalf("系统列: %#v, err = %v", byColumn, err)
	}
	byField, err := gcm.GetNodes(NodeQuery{Type: "category", Where: gql.P("=", "$address", "news")}, 0, 0)
	if err != nil || len(byField) != 1 || byField[0].ID != news {
		t.Fatalf("字段路径: %#v, err = %v", byField, err)
	}
	// 排序也认系统列（能力来自 systemFields 声明）
	sorted, err := gcm.GetNodes(NodeQuery{Type: "category",
		Sort: []gql.SortField{{Path: gql.Path{Kind: gql.PathSystem, Field: "address"}}}}, 0, 0)
	if err != nil || len(sorted) != 2 {
		t.Fatalf("按地址排序: %#v, err = %v", sorted, err)
	}
	if sorted[0].Fields.Str("address") != "news" {
		t.Fatalf("排序结果 = %#v", sorted[0].Fields)
	}
}

// GetNodes: 条件 + 排序 + 分页; nil Where = 不过滤。
func TestGetNodes(t *testing.T) {
	gcm, _, _, _, _, _ := seed(t)

	// 不过滤
	all, err := gcm.GetNodes(NodeQuery{Type: "article"}, 0, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("all = %d, err = %v", len(all), err)
	}
	// 条件
	done, err := gcm.GetNodes(NodeQuery{
		Type:  "article",
		Where: gql.P("=", "$state", "published"),
	}, 0, 0)
	if err != nil || len(done) != 2 {
		t.Fatalf("published = %d, err = %v", len(done), err)
	}
	// 排序 + 稳定序（同 views 不分页时按 id DESC 收尾）
	sorted, err := gcm.GetNodes(NodeQuery{
		Type: "article",
		Sort: []gql.SortField{{Path: gql.Path{Kind: gql.PathField, Field: "views"}, Desc: true}},
	}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	views := []int64{sorted[0].Fields.Int("views"), sorted[1].Fields.Int("views"), sorted[2].Fields.Int("views")}
	if views[0] != 30 || views[1] != 20 || views[2] != 10 {
		t.Fatalf("排序 = %#v", views)
	}
	// 分页
	page, err := gcm.GetNodes(NodeQuery{Type: "article",
		Sort: []gql.SortField{{Path: gql.Path{Kind: gql.PathField, Field: "views"}}}}, 2, 1)
	if err != nil || len(page) != 2 {
		t.Fatalf("分页 = %d, err = %v", len(page), err)
	}
	if page[0].Fields.Int("views") != 20 {
		t.Fatalf("第二页 = %#v", page[0].Fields)
	}
}

// 读出来的引用值必须能原样写回（读→写的往返）—— 这是读投影存在的理由。
func TestReadWriteRoundTrip(t *testing.T) {
	gcm, _, _, _, a2, _ := seed(t)
	before, err := gcm.GetNode(a2)
	if err != nil {
		t.Fatal(err)
	}
	revision := before.Revision
	err = gcm.PatchNode(nil, a2, &NodePatch{
		Revision: &revision,
		Fields: map[string]any{
			"categories": before.Fields["categories"], // 原样写回
			"title":      "乙改",
		},
	})
	if err != nil {
		t.Fatalf("读出来的值写回失败: %v", err)
	}
	after, err := gcm.GetNode(a2)
	if err != nil {
		t.Fatal(err)
	}
	cats := after.Fields["categories"].([]int64)
	if len(cats) != 2 || cats[0] != before.Fields["categories"].([]int64)[0] {
		t.Fatalf("往返后引用变了: %#v → %#v", before.Fields["categories"], cats)
	}
}

// GetNodesByIDs: 按请求顺序、缺失的跳过。
func TestGetNodesByIDs(t *testing.T) {
	gcm, _, _, a1, a2, a3 := seed(t)
	nodes, err := gcm.GetNodesByIDs([]int64{a3, 9999, a1})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].ID != a3 || nodes[1].ID != a1 {
		t.Fatalf("顺序/缺失处理 = %#v", nodes)
	}
	empty, err := gcm.GetNodesByIDs(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空入参 = %#v, err = %v", empty, err)
	}
	_ = a2
}

// CountNodes: 上限语义（超上限返回上限值）, CountExact 精确。
func TestCountNodes(t *testing.T) {
	gcm, _, _, _, _, _ := seed(t)
	exact, err := gcm.CountNodes(NodeQuery{Type: "article"}, CountExact)
	if err != nil || exact != 3 {
		t.Fatalf("exact = %d, err = %v", exact, err)
	}
	limited, err := gcm.CountNodes(NodeQuery{Type: "article"}, 2)
	if err != nil || limited != 2 {
		t.Fatalf("上限截断 = %d, err = %v", limited, err)
	}
	withWhere, err := gcm.CountNodes(NodeQuery{Type: "article", Where: gql.P("=", "$state", "published")}, 0)
	if err != nil || withWhere != 2 {
		t.Fatalf("带条件 = %d, err = %v", withWhere, err)
	}
}

// 参数与类型校验: 必须有 type; 上限; 类型不存在。
func TestReadQueryValidation(t *testing.T) {
	gcm := openFixture(t)
	if _, err := gcm.GetNodes(NodeQuery{}, 0, 0); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("缺 type: %v", err)
	}
	if _, err := gcm.GetNodes(NodeQuery{Type: "article"}, MaxPageSize+1, 0); !errors.Is(err, ErrQueryTooComplex) {
		t.Fatalf("页大小上限: %v", err)
	}
	if _, err := gcm.GetNodes(NodeQuery{Type: "article"}, -1, 0); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("负数: %v", err)
	}
	if _, err := gcm.GetNodes(NodeQuery{Type: "ghost"}, 0, 0); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("类型不存在: %v", err)
	}
	if _, err := gcm.CountNodes(NodeQuery{Type: "ghost"}, 0); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("计数: %v", err)
	}
}

// 排序能力来自声明: text 可排, 系统列可排, 关系不可排, 未声明的不可排。
func TestSortCapability(t *testing.T) {
	gcm, _, _, _, _, _ := seed(t)
	cases := []struct {
		name  string
		field gql.Path
		want  error
	}{
		{"字段可排", gql.Path{Kind: gql.PathField, Field: "title"}, nil},
		{"系统列可排", gql.Path{Kind: gql.PathSystem, Field: "id"}, nil},
		{"关系不可排", gql.Path{Kind: gql.PathOutRef, Field: "categories"}, ErrInvalidField},
		{"不存在的字段", gql.Path{Kind: gql.PathField, Field: "ghost"}, ErrInvalidField},
		{"不存在的系统列", gql.Path{Kind: gql.PathSystem, Field: "ghost"}, ErrInvalidField},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := gcm.GetNodes(NodeQuery{Type: "article", Sort: []gql.SortField{{Path: test.field}}}, 0, 0)
			if !errors.Is(err, test.want) {
				t.Fatalf("err = %v, want %v", err, test.want)
			}
		})
	}

	// 重复排序字段 / 超过 8 个
	duplicate := []gql.SortField{
		{Path: gql.Path{Kind: gql.PathField, Field: "title"}},
		{Path: gql.Path{Kind: gql.PathField, Field: "title"}},
	}
	if _, err := gcm.GetNodes(NodeQuery{Type: "article", Sort: duplicate}, 0, 0); !errors.Is(err, ErrInvalidField) {
		t.Fatalf("重复: %v", err)
	}
	many := make([]gql.SortField, 9)
	for i := range many {
		many[i] = gql.SortField{Path: gql.Path{Kind: gql.PathSystem, Field: "id"}}
	}
	if _, err := gcm.GetNodes(NodeQuery{Type: "article", Sort: many}, 0, 0); !errors.Is(err, ErrQueryTooComplex) {
		t.Fatalf("超过 8 个: %v", err)
	}
}

// 条件里的未知算符/坏字段在读路径同样 fail-loud（编译器同一个入口）。
func TestReadWhereValidation(t *testing.T) {
	gcm := openFixture(t)
	if _, err := gcm.GetNodes(NodeQuery{Type: "article", Where: gql.P("match", "x")}, 0, 0); !errors.Is(err, ErrInvalidOperator) {
		t.Fatalf("未知算符: %v", err)
	}
	if _, err := gcm.GetNodes(NodeQuery{Type: "article", Where: gql.P("=", "$ghost", "x")}, 0, 0); !errors.Is(err, ErrInvalidField) {
		t.Fatalf("坏字段: %v", err)
	}
}
