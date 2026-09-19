package so

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 三种写法产出**同一棵 AST** —— 任何一条前端跑偏（切片没归一、数字类型不一致、
// 空项处理不同）都在这里炸。
func TestThreeFrontendsAgree(t *testing.T) {
	ids := []int64{1, 2, 3}

	built := AND(
		P("=", "$state", "published"),
		P(">", "$views", 100),
		OR(
			P("in", "->categories", ids),
			P("exists", "<-article.categories"),
			REF("->categories", P("=", "$slug", "news")),
		),
		NOT(P("missing", "$body")),
		IF(false, P("=", "$never", 1)), // ← 空项: 等价于下面文本里"干脆不写"
		P("contains", "display", "文"),
		P("match", "人工智能"), // 未注册的算符: 名字+参数原样进 AST
	)

	lisp, err := ParseLisp(`(and
		(= $state "published")
		(> $views 100)
		(or
			(in ->categories [1 2 3])
			(exists <-article.categories)
			(ref ->categories (= $slug "news"))
		)
		(not (missing $body))
		(contains display "文")
		(match "人工智能")
	)`, nil)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(built.Tree())
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := ParseJSON(string(raw))
	if err != nil {
		t.Fatal(err)
	}

	fromGo, err := built.Expr()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromGo, lisp) {
		t.Fatalf("Go ≠ lisp\n Go:   %#v\n lisp: %#v", fromGo, lisp)
	}
	if !reflect.DeepEqual(fromGo, fromJSON) {
		t.Fatalf("Go ≠ json\n Go:   %#v\n json: %#v", fromGo, fromJSON)
	}
}

// AST 只有两种节点: Logic（组合）与 Predicate（其它一切）。
func TestASTShapes(t *testing.T) {
	expr, err := AND(
		P("=", "$state", "published"),
		NOT(P("missing", "$body")),
		REF("->categories", P("=", "$slug", "news")),
	).Expr()
	if err != nil {
		t.Fatal(err)
	}
	want := Logic{Op: OpAnd, Args: []Expr{
		Predicate{Name: "=", Args: []any{"$state", "published"}},
		Logic{Op: OpNot, Args: []Expr{
			Predicate{Name: "missing", Args: []any{"$body"}},
		}},
		// ref 的子表达式保持**原始树** —— 它由算符在编译期按位置解释
		Predicate{Name: "ref", Args: []any{
			"->categories",
			[]any{"=", "$slug", "news"},
		}},
	}}
	if !reflect.DeepEqual(expr, want) {
		t.Fatalf("ast = %#v\nwant %#v", expr, want)
	}
}

// 构造器产出的前端树（这就是 json.Marshal 出去的东西）。
func TestTreeShape(t *testing.T) {
	got := AND(
		P("=", "$state", "published"),
		OR(P("in", "->categories", []int{1, 2}), P("exists", "$body")),
	).Tree()
	want := []any{"and",
		[]any{"=", "$state", "published"},
		[]any{"or",
			[]any{"in", "->categories", []any{int64(1), int64(2)}},
			[]any{"exists", "$body"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree = %#v\nwant %#v", got, want)
	}
}

// IF(false) = 空项, 构造期丢掉 —— 树里不会出现 null。
func TestIfDropsItem(t *testing.T) {
	if !IF(false, P("=", "$a", 1)).absent() {
		t.Fatal("IF(false) 必须是空项")
	}
	if IF(true, P("=", "$a", int64(1))).Tree() == nil {
		t.Fatal("条件成立时必须是真的节点")
	}

	got := AND(P("=", "$state", "published"), IF(false, P("=", "$author", 7))).Tree()
	want := []any{"and", []any{"=", "$state", "published"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree = %#v\nwant %#v", got, want)
	}
}

// IF 收**任意节点** —— 叶子或整组条件。
func TestIfTakesAnyNode(t *testing.T) {
	got := AND(
		P("=", "$state", "published"),
		IF(true, OR(P("=", "$state", "draft"), P("=", "$author", int64(7)))),
		IF(false, AND(P("=", "$a", 1), P("=", "$b", 2))),
	).Tree()
	want := []any{"and",
		[]any{"=", "$state", "published"},
		[]any{"or", []any{"=", "$state", "draft"}, []any{"=", "$author", int64(7)}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree = %#v\nwant %#v", got, want)
	}
}

// 空项穿过 NOT / REF 仍然是空项 —— 包一层不会把"没有条件"变成"一个条件"。
func TestAbsentPropagatesThroughWrappers(t *testing.T) {
	absent := IF(false, P("=", "$a", 1))
	if !NOT(absent).absent() {
		t.Fatal("NOT(空项) 必须是空项")
	}
	if !REF("->categories", absent).absent() {
		t.Fatal("REF(空项) 必须是空项")
	}
	got := AND(P("=", "$state", "published"), IF(false, NOT(P("=", "$author", int64(7))))).Tree()
	want := []any{"and", []any{"=", "$state", "published"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree = %#v", got)
	}
}

// 切片归一: []int64 → []any（否则编译期会把集合当成单个值）。
func TestTreeNormalizesSlices(t *testing.T) {
	tree := P("in", "->categories", []int64{1, 2, 3}).Tree().([]any)
	values, ok := tree[2].([]any)
	if !ok || len(values) != 3 || values[0] != int64(1) {
		t.Fatalf("values = %#v", tree[2])
	}
	nested := P("in", "$a", [][]int{{1, 2}}).Tree().([]any)[2].([]any)
	if _, ok := nested[0].([]any); !ok {
		t.Fatalf("嵌套切片没归一: %#v", nested)
	}
}

// Go 的 int 字面量也归一成 int64 —— 与 Lisp/JSON 一致。
func TestTreeNormalizesNumbers(t *testing.T) {
	tree := P(">", "$views", 100).Tree().([]any)
	if tree[2] != int64(100) {
		t.Fatalf("int 必须归一成 int64: %#v", tree[2])
	}
}

// ── 结构校验（复杂度上限与形态）─────────────────

// 空 AND/OR 是放行方向 —— 结构层就要响, 不能静默恒真。
func TestEmptyGroupFails(t *testing.T) {
	if _, err := AND().Expr(); err == nil || !strings.Contains(err.Error(), "requires arguments") {
		t.Fatalf("err = %v", err)
	}
	if _, err := OR().Expr(); err == nil {
		t.Fatal("空 OR 必须报错")
	}
	if _, err := NOT(P("=", "$a", 1)).Expr(); err != nil {
		t.Fatalf("单个 not 合法: %v", err)
	}
}

// 条件位置必须是数组; 顶层 nil / 值都不行。
func TestMalformedTrees(t *testing.T) {
	cases := []struct {
		tree any
		want string
	}{
		{nil, "expression required"},
		{[]any{}, "empty condition"},
		{[]any{123}, "must be a non-empty string"},
		{[]any{"and", 1}, "condition must be"},
		{"plain value", "condition must be"},
		{[]any{"not"}, "not takes 1 argument"},
		{[]any{"not", []any{"=", "$a", 1}, []any{"=", "$b", 2}}, "not takes 1 argument"},
	}
	for _, test := range cases {
		_, err := NewReader().Read(test.tree)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%#v: err = %v, want %q", test.tree, err, test.want)
		}
	}
}

// 节点数与深度上限（前端输入不可信, 必须封顶）。
func TestReadLimits(t *testing.T) {
	var deep any = []any{"=", "$a", int64(1)}
	for i := 0; i <= MaxFilterDepth; i++ {
		deep = []any{"not", deep}
	}
	if _, err := NewReader().Read(deep); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("深度上限: err = %v", err)
	}

	wide := []any{"and"}
	for i := 0; i <= MaxFilterNodes; i++ {
		wide = append(wide, []any{"=", "$a", int64(1)})
	}
	if _, err := NewReader().Read(wide); err == nil || !strings.Contains(err.Error(), "nodes") {
		t.Fatalf("节点上限: err = %v", err)
	}
}

// 语言里没有 null 这条语义 —— 条件包含只在构造期发生（IF）。
func TestLanguageHasNoNull(t *testing.T) {
	cases := []string{
		`(and (= $a 1) null)`,
		`(or null)`,
		`(not null)`,
		`null`,
	}
	for _, src := range cases {
		if _, err := ParseLisp(src, nil); err == nil {
			t.Fatalf("lisp %s must fail", src)
		}
	}
	if _, err := ParseJSON(`["and", ["=", "$a", 1], null]`); err == nil {
		t.Fatal("json null must fail")
	}
	// 值位置的 null 不受影响
	if _, err := ParseLisp(`(= $published_at null)`, nil); err != nil {
		t.Fatalf("值位置 null: %v", err)
	}
}
