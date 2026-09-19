package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kran/dba"
	"github.com/kran/gcmv3/types"
	_ "modernc.org/sqlite"
)

const writeTypesYAML = `
types:
  category:
    capabilities: { addressable: true }
    fields:
      - { name: name, kind: text }
      - { name: parent, kind: ref, to: category, transitive: true }
  person:
    fields:
      - { name: name, kind: text }
      - { name: admin, kind: bool, default: false }
      - { name: mentor, kind: ref, to: person }
  article:
    capabilities: { addressable: true }
    fields:
      - { name: title, kind: text }
      - { name: state, kind: select, options: [draft, published], default: draft }
      - { name: views, kind: number, default: 0 }
      - { name: featured, kind: bool }
      - { name: categories, kind: "refs", to: category }
      - { name: authors, kind: "refs", to: person }
`

// applySchema 把 core/migrations/*.sql 的 Up 段跑上去。
//
// 迁移执行器还没做（不在这一步的范围里）—— 所以测试自己把 SQL 执行一遍,
// schema 的唯一来源仍然是那些 .sql 文件。
func applySchema(t *testing.T, db *dba.SQL) {
	t.Helper()
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join("migrations", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var up []string
		inUp := false
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(trimmed, "-- +goose Down"):
				inUp = false
			case strings.HasPrefix(trimmed, "-- +goose"):
				inUp = true
			case inUp && trimmed != "":
				up = append(up, line)
			}
		}
		_, err = db.Add(strings.Join(up, "\n")).Exec()
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
	}
}

// openFixture 真机: 临时库（WAL + foreign_keys + busy_timeout）→ 建表 → 引擎起来。
func openFixture(t *testing.T) *GCM {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "gcm.sqlite") +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := dba.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	applySchema(t, db)

	ts := types.New()
	err = ts.Load([]byte(writeTypesYAML))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := OpenGCM(db, ts)
	if err != nil {
		t.Fatal(err)
	}
	return gcm
}

func nodeCount(t *testing.T, gcm *GCM, typeName string) int64 {
	t.Helper()
	count, err := gcm.db.Add(`SELECT COUNT(1) FROM nodes WHERE type = #{1}`, typeName).FetchOne[int64]()
	if err != nil {
		t.Fatal(err)
	}
	if count == nil {
		return 0
	}
	return *count
}

// edgesOf 直接查边表。读 API 还没做, 所以断言的是**表本身** ——
// 写路径的产物就是这些行, 这么断言反而更贴。
func edgesOf(t *testing.T, gcm *GCM, from int64, field string) []Edge {
	t.Helper()
	edges, err := gcm.db.Add(
		`SELECT * FROM edges WHERE from_node = #{1} AND field = #{2} ORDER BY sort, id`,
		from, field).FetchList[Edge]()
	if err != nil {
		t.Fatal(err)
	}
	return edges
}

func edgeCount(t *testing.T, gcm *GCM) int64 {
	t.Helper()
	count, err := gcm.db.Add(`SELECT COUNT(1) FROM edges`).FetchOne[int64]()
	if err != nil {
		t.Fatal(err)
	}
	if count == nil {
		return 0
	}
	return *count
}

// 建节点: 默认值 / 标量落 fields / 引用落边（不落 fields）。
func TestCreateNode(t *testing.T) {
	gcm := openFixture(t)

	cat, err := gcm.CreateNode(nil, &Node{Type: "category",
		Fields: Fields{"name": "新闻", "address": "news"}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := gcm.CreateNode(nil, &Node{Type: "article",
		Fields: Fields{"title": "甲", "categories": []any{cat}, "views": int64(3)}})
	if err != nil {
		t.Fatal(err)
	}

	row, err := gcm.nodeRow(nil, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Revision != 1 {
		t.Fatalf("revision = %d", row.Revision)
	}
	if row.CreatedAt == 0 || row.UpdatedAt != row.CreatedAt {
		t.Fatalf("时间列 = %d / %d", row.CreatedAt, row.UpdatedAt)
	}
	// 默认值落库
	if row.Fields.Str("state") != "draft" || row.Fields.Int("views") != 3 {
		t.Fatalf("fields = %#v", row.Fields)
	}
	// 引用**不在** fields 里 —— 它是边
	if _, ok := row.Fields["categories"]; ok {
		t.Fatalf("引用不该进 fields: %#v", row.Fields)
	}
	if _, ok := row.Fields["authors"]; ok {
		t.Fatalf("未提供的引用不该出现: %#v", row.Fields)
	}
	// 边落对了
	edges := edgesOf(t, gcm, id, "categories")
	if len(edges) != 1 {
		t.Fatalf("出边 = %#v", edges)
	}
	if edges[0].ToNode != cat {
		t.Fatalf("edge = %#v", edges[0])
	}
}

// 校验失败一律 422 语义（ErrInvalidFields），且不落任何行。
func TestCreateNodeValidation(t *testing.T) {
	gcm := openFixture(t)
	cases := []struct {
		name string
		node *Node
	}{
		{"未知类型", &Node{Type: "ghost"}},
		{"未知字段", &Node{Type: "article", Fields: Fields{"nope": 1}}},
		{"字段类型不对", &Node{Type: "article", Fields: Fields{"views": "十"}}},
		{"select 值非法", &Node{Type: "article", Fields: Fields{"state": "wat"}}},
		{"引用目标不存在", &Node{Type: "article", Fields: Fields{"categories": []any{int64(999)}}}},
		{"引用重复目标", &Node{Type: "article", Fields: Fields{"categories": []any{int64(1), int64(1)}}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := gcm.CreateNode(nil, test.node)
			if err == nil {
				t.Fatal("必须报错")
			}
			if nodeCount(t, gcm, test.node.Type) != 0 {
				t.Fatal("失败不该留下行")
			}
		})
	}
}

// 引用目标类型不对（跨类型引用）也要挡住, 而且是 500 级（不是字段校验错）。
func TestCreateNodeTargetType(t *testing.T) {
	gcm := openFixture(t)
	person, err := gcm.CreateNode(nil, &Node{Type: "person", Fields: Fields{"name": "人"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = gcm.CreateNode(nil, &Node{Type: "article",
		Fields: Fields{"title": "x", "categories": []any{person}}})
	if err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("err = %v", err)
	}
	if edgeCount(t, gcm) != 0 {
		t.Fatal("目标类型不对不该落边")
	}
}

// 地址空间是**全表**的: 同一个地址落在两个类型上也算撞车（唯一索引不带类型谓词）。
// 与"没有地址"无关 —— NULL 互不相等。
func TestAddressUniqueness(t *testing.T) {
	gcm := openFixture(t)
	_, err := gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "一", "address": "dup"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "二", "address": "dup"}})
	if err == nil {
		t.Fatal("同类型重复地址必须被拒")
	}
	// 跨类型同名也是撞车（地址空间不分类型）
	_, err = gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{"title": "甲", "address": "dup"}})
	if err == nil {
		t.Fatal("跨类型重复地址必须被拒")
	}
	// 没有地址的行互不影响: 缺字段与**显式空串**都算"没有地址"
	//（空串是关键那条 —— 裸 json_extract 会存成 '' ⇒ 两条空地址撞车）
	没有地址 := []Fields{
		{"title": "无地址甲"},
		{"title": "无地址乙"},
		{"title": "空串甲", "address": ""},
		{"title": "空串乙", "address": ""},
	}
	for _, fields := range 没有地址 {
		_, err = gcm.CreateNode(nil, &Node{Type: "article", Fields: fields})
		if err != nil {
			t.Fatalf("%v: %v", fields, err)
		}
	}
	// 换个地址就能建
	if _, err := gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{"title": "丙", "address": "fresh"}}); err != nil {
		t.Fatal(err)
	}
}

// 差量更新: fields 用 json_patch 合并（没给的字段保留）。
func TestPatchNode(t *testing.T) {
	gcm := openFixture(t)
	id, err := gcm.CreateNode(nil, &Node{Type: "article",
		Fields: Fields{"title": "甲", "views": int64(3)}})
	if err != nil {
		t.Fatal(err)
	}

	revision := int64(1)
	err = gcm.PatchNode(nil, id, &NodePatch{
		Revision: &revision,
		Fields:   map[string]any{"views": int64(9), "title": "甲改"},
	})
	if err != nil {
		t.Fatal(err)
	}
	row, _ := gcm.nodeRow(nil, id)
	if row.Fields.Int("views") != 9 || row.Fields.Str("title") != "甲改" {
		t.Fatalf("patch 结果 = %#v", row)
	}
	if row.Revision != 2 {
		t.Fatalf("revision = %d, want 2", row.Revision)
	}
	if row.UpdatedAt < row.CreatedAt {
		t.Fatalf("updated_at 没推进: %d < %d", row.UpdatedAt, row.CreatedAt)
	}
}

// 乐观锁: 版本不对报 ErrRevisionConflict, 且不改任何东西。
func TestPatchRevisionConflict(t *testing.T) {
	gcm := openFixture(t)
	id, _ := gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{"title": "甲"}})

	stale := int64(0)
	err := gcm.PatchNode(nil, id, &NodePatch{Revision: &stale, Fields: map[string]any{"title": "改"}})
	if !errors.Is(err, ErrRevisionConflict) && !errors.Is(err, ErrInvalidFields) {
		t.Fatalf("err = %v", err)
	}

	wrong := int64(99)
	err = gcm.PatchNode(nil, id, &NodePatch{Revision: &wrong, Fields: map[string]any{"title": "改"}})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("err = %v", err)
	}
	row, _ := gcm.nodeRow(nil, id)
	if row.Fields.Str("title") != "甲" || row.Revision != 1 {
		t.Fatalf("冲突不该改动: %#v", row)
	}
}

// 空 patch 幂等（零 UPDATE）; nil patch 报错。
func TestPatchNoop(t *testing.T) {
	gcm := openFixture(t)
	id, _ := gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{"title": "甲"}})
	revision := int64(1)
	err := gcm.PatchNode(nil, id, &NodePatch{Revision: &revision})
	if err != nil {
		t.Fatal(err)
	}
	row, _ := gcm.nodeRow(nil, id)
	if row.Revision != 1 {
		t.Fatalf("空 patch 不该升版本: %d", row.Revision)
	}
	if err := gcm.PatchNode(nil, id, nil); err == nil {
		t.Fatal("nil patch 必须报错")
	}
	missing := &NodePatch{Revision: &revision, Fields: map[string]any{"title": "x"}}
	if err := gcm.PatchNode(nil, int64(999), missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// 引用字段的 patch 是**替换**（先清旧边再落新边）, 不是叠边。
func TestPatchRefReplacement(t *testing.T) {
	gcm := openFixture(t)
	a, _ := gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "A"}})
	b, _ := gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "B"}})
	id, _ := gcm.CreateNode(nil, &Node{Type: "article",
		Fields: Fields{"title": "文", "categories": []any{a}}})

	revision := int64(1)
	err := gcm.PatchNode(nil, id, &NodePatch{Revision: &revision, Fields: map[string]any{"categories": []any{b}}})
	if err != nil {
		t.Fatal(err)
	}
	edges := edgesOf(t, gcm, id, "categories")
	if len(edges) != 1 || edges[0].ToNode != b {
		t.Fatalf("必须替换成 B: %#v", edges)
	}

	// 清空引用
	revision = 2
	err = gcm.PatchNode(nil, id, &NodePatch{Revision: &revision, Fields: map[string]any{"categories": []any{}}})
	if err != nil {
		t.Fatal(err)
	}
	if remaining := edgesOf(t, gcm, id, "categories"); len(remaining) != 0 {
		t.Fatalf("清空后还有 %d 条边", len(remaining))
	}
}

// 删除: 有入引用就不许删, 错误里带引用方; 没引用则连带清掉自己的出边。
func TestDeleteNode(t *testing.T) {
	gcm := openFixture(t)
	cat, _ := gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "分类"}})
	art, _ := gcm.CreateNode(nil, &Node{Type: "article",
		Fields: Fields{"title": "文", "categories": []any{cat}}})

	// 分类被文章引用 ⇒ 拒绝, 并指出引用方
	err := gcm.DeleteNode(nil, cat)
	var restricted *DeleteRestrictedError
	if !errors.As(err, &restricted) {
		t.Fatalf("err = %v", err)
	}
	if len(restricted.References) != 1 ||
		restricted.References[0].FromNode != art ||
		restricted.References[0].Field != "categories" {
		t.Fatalf("引用方 = %#v", restricted.References)
	}
	if restricted.References[0].ID == 0 {
		t.Fatalf("edge id 要带上: %#v", restricted.References[0])
	}
	// 解除引用之后就能删（限制只针对"还被引用着"）
	revision, err := gcm.GetNode(art)
	if err != nil {
		t.Fatal(err)
	}
	rev := revision.Revision
	err = gcm.PatchNode(nil, art, &NodePatch{
		Revision: &rev, Fields: map[string]any{"categories": []any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if nodeCount(t, gcm, "category") != 1 {
		t.Fatal("拒绝删除后节点必须还在")
	}

	// 文章没有入引用 ⇒ 删掉, 出边一起清
	err = gcm.DeleteNode(nil, art)
	if err != nil {
		t.Fatal(err)
	}
	if nodeCount(t, gcm, "article") != 0 || edgeCount(t, gcm) != 0 {
		t.Fatalf("删除后 nodes=%d edges=%d", nodeCount(t, gcm, "article"), edgeCount(t, gcm))
	}

	// 不存在 / 重复删
	if err := gcm.DeleteNode(nil, art); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// 引用是**有向**的: A→B 与 B→A 是两条独立的边（无对称存储语义）。
func TestRefIsDirected(t *testing.T) {
	gcm := openFixture(t)
	a, _ := gcm.CreateNode(nil, &Node{Type: "person", Fields: Fields{"name": "A"}})
	b, _ := gcm.CreateNode(nil, &Node{Type: "person",
		Fields: Fields{"name": "B", "mentor": a}})

	edges := edgesOf(t, gcm, b, "mentor")
	if len(edges) != 1 || edges[0].ToNode != a {
		t.Fatalf("边 = %#v", edges)
	}
	// 反向是**另一条边**（不是同一对的规范化写法）
	if reverse := edgesOf(t, gcm, a, "mentor"); len(reverse) != 0 {
		t.Fatalf("反向不该存在: %#v", reverse)
	}

	// 同一对再来一条: UNIQUE(from_node, field, to_node) 挡掉
	//（两个并发写入靠它分胜负 —— 应用层检查挡不住）
	_, err := gcm.db.Insert("edges", map[string]any{
		"from_node": b, "field": "mentor", "to_node": a,
		"sort": 0, "created_at": nowValue(),
	}).Exec()
	if err == nil {
		t.Fatal("重复边必须被拒")
	}
}

// 传递引用不许成环; 自引用也不许。
func TestTransitiveCycle(t *testing.T) {
	gcm := openFixture(t)
	root, _ := gcm.CreateNode(nil, &Node{Type: "category", Fields: Fields{"name": "root"}})
	child, err := gcm.CreateNode(nil, &Node{Type: "category",
		Fields: Fields{"name": "child", "parent": root}})
	if err != nil {
		t.Fatal(err)
	}

	// 自引用
	revision := int64(1)
	err = gcm.PatchNode(nil, root, &NodePatch{Revision: &revision, Fields: map[string]any{"parent": root}})
	if err == nil {
		t.Fatal("自引用必须被拒")
	}
	// 成环: root.parent = child （child.parent = root 已存在）
	err = gcm.PatchNode(nil, root, &NodePatch{Revision: &revision, Fields: map[string]any{"parent": child}})
	if err == nil {
		t.Fatal("成环必须被拒")
	}
	// 环被拒之后不该留下半条边
	if edges := edgesOf(t, gcm, root, "parent"); len(edges) != 0 {
		t.Fatalf("失败的写入不该留边: %#v", edges)
	}
}

// 启动期 DDL 幂等: 声明式索引重跑不炸。
func TestOpenIsIdempotent(t *testing.T) {
	gcm := openFixture(t)
	_, err := gcm.CreateNode(nil, &Node{Type: "article", Fields: Fields{"title": "甲", "address": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	// 引擎不做启动期 DDL ⇒ 再建一次不该碰任何东西
	if _, err := OpenGCM(gcm.db, gcm.types); err != nil {
		t.Fatal(err)
	}
	// 地址唯一索引来自**迁移**（静态 DDL）
	var count int64
	row, err := gcm.db.Add(`SELECT COUNT(1) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_nodes_address'`).FetchOne[int64]()
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		count = *row
	}
	if count != 1 {
		t.Fatalf("迁移里该建出 idx_nodes_address, 实际 %d", count)
	}
}
