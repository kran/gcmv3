package so

import (
	"reflect"
	"strings"
	"testing"
)

func mustLisp(t *testing.T, src string, params map[string]any) Expr {
	t.Helper()
	expr, err := ParseLisp(src, params)
	if err != nil {
		t.Fatalf("ParseLisp(%q): %v", src, err)
	}
	return expr
}

// 一个覆盖全部基础算符的表达式 → 期望的 AST。
func TestParseLispAST(t *testing.T) {
	src := `(and
		(= $state "published")
		(> $views 100)
		(or
			(in ->categories [1 2 3])
			(exists <-article.categories)
			(ref ->categories (= $slug "news"))
		)
		(not (missing $body))
		(contains display "文")
		(true)
	)`
	want := Logic{Op: OpAnd, Args: []Expr{
		Predicate{Name: "=", Args: []any{"$state", "published"}},
		Predicate{Name: ">", Args: []any{"$views", int64(100)}},
		Logic{Op: OpOr, Args: []Expr{
			Predicate{Name: "in", Args: []any{"->categories", []any{int64(1), int64(2), int64(3)}}},
			Predicate{Name: "exists", Args: []any{"<-article.categories"}},
			Predicate{Name: "ref", Args: []any{"->categories", []any{"=", "$slug", "news"}}},
		}},
		Logic{Op: OpNot, Args: []Expr{
			Predicate{Name: "missing", Args: []any{"$body"}},
		}},
		Predicate{Name: "contains", Args: []any{"display", "文"}},
		Predicate{Name: "true"},
	}}
	got := mustLisp(t, src, nil)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ast mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// 未注册的名字照样能写: 它就是一个名字 + 参数, 认不认由编译期决定。
func TestUnknownOperatorIsJustAName(t *testing.T) {
	expr := mustLisp(t, `(match "人工智能")`, nil)
	predicate, ok := expr.(Predicate)
	if !ok || predicate.Name != "match" || len(predicate.Args) != 1 {
		t.Fatalf("got %#v", expr)
	}
	if predicate.Args[0] != "人工智能" {
		t.Fatalf("args = %#v", predicate.Args)
	}
	// 拼错算符名也走同一条路
	typo := mustLisp(t, `(adn (= $state "x"))`, nil)
	if predicate, ok := typo.(Predicate); !ok || predicate.Name != "adn" {
		t.Fatalf("got %#v", typo)
	}
}

// 占位符 {:name}: 只在文本前端存在, 解析在 tokenizer 这一侧。
func TestPlaceholders(t *testing.T) {
	params := map[string]any{"state": "published", "owners": []any{7, 8}}
	got := mustLisp(t, `(and (= $state {:state}) (in ->owner {:owners}))`, params)
	want := Logic{Op: OpAnd, Args: []Expr{
		Predicate{Name: "=", Args: []any{"$state", "published"}},
		Predicate{Name: "in", Args: []any{"->owner", []any{int64(7), int64(8)}}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestPlaceholderUnbound(t *testing.T) {
	_, err := ParseLisp(`(= $state {:missing})`, map[string]any{"state": "x"})
	if err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("err = %v", err)
	}
}

// 值的形态: 数字/字符串/布尔/null/数组。
func TestLispValues(t *testing.T) {
	cases := []struct {
		src  string
		want []any
	}{
		{`(= $a 1)`, []any{"$a", int64(1)}},
		{`(= $a -2.5)`, []any{"$a", -2.5}},
		{`(= $a "x")`, []any{"$a", "x"}},
		{`(= $a true)`, []any{"$a", true}},
		{`(= $a null)`, []any{"$a", nil}},
		{`(in $a [1 "x" true])`, []any{"$a", []any{int64(1), "x", true}}},
	}
	for _, test := range cases {
		expr := mustLisp(t, test.src, nil)
		predicate, ok := expr.(Predicate)
		if !ok || !reflect.DeepEqual(predicate.Args, test.want) {
			t.Fatalf("%s → %#v, want %#v", test.src, expr, test.want)
		}
	}
}

func TestLispSyntaxErrors(t *testing.T) {
	cases := []struct{ src, want string }{
		{`(= $a 1))`, "trailing input"},
		{`(and (= $a 1)`, "unterminated ("},
		{`(in $a [1 2`, "unterminated ["},
		{`(in $a [1 2)`, "unexpected )"},
		{`(= $a "x)`, "unterminated string"},
		{`(in $a [(= $b 1)])`, "array elements must be values"},
		{``, "unexpected end"},
		{`()`, "empty token"},
	}
	for _, test := range cases {
		_, err := ParseLisp(test.src, nil)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%q: err = %v, want %q", test.src, err, test.want)
		}
	}
}

// 上限: 字节数与数组元素个数（节点/深度在 Reader 那一层）。
func TestLispLimits(t *testing.T) {
	if _, err := ParseLisp(strings.Repeat("x", MaxFilterBytes+1), nil); err == nil {
		t.Fatal("byte limit must fail")
	}
	items := make([]string, MaxSetValues+1)
	for i := range items {
		items[i] = "1"
	}
	src := "(in $a [" + strings.Join(items, " ") + "])"
	if _, err := ParseLisp(src, nil); err == nil {
		t.Fatal("array size limit must fail")
	}
}
