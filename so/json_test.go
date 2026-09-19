package so

import (
	"reflect"
	"strings"
	"testing"
)

// JSON 数组前端与 Lisp 是同一棵树 —— "同一查询两种写法"的对照。
func TestJSONEqualsLisp(t *testing.T) {
	raw := `["and",
		["=", "$state", "published"],
		[">", "$views", 100],
		["or",
			["in", "->categories", [1, 2, 3]],
			["exists", "<-article.categories"],
			["ref", "->categories", ["=", "$slug", "news"]]
		],
		["not", ["missing", "$body"]],
		["contains", "display", "文"]
	]`
	lisp := `(and
		(= $state "published")
		(> $views 100)
		(or
			(in ->categories [1 2 3])
			(exists <-article.categories)
			(ref ->categories (= $slug "news"))
		)
		(not (missing $body))
		(contains display "文")
	)`
	fromJSON, err := ParseJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	fromLisp, err := ParseLisp(lisp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromJSON, fromLisp) {
		t.Fatalf("两种前端必须产出逐位相同的树\n json: %#v\n lisp: %#v", fromJSON, fromLisp)
	}
}

// 数字归一: JSON 用 json.Number 解码（保 int64 精度）, 到算符手里已经是
// int64（整数值）或 float64（带小数）—— 与 Lisp 一致。
func TestJSONNumberNormalization(t *testing.T) {
	expr, err := ParseJSON(`["and", ["=", "id", 9007199254740993], ["=", "$rate", 1.5]]`)
	if err != nil {
		t.Fatal(err)
	}
	logic, ok := expr.(Logic)
	if !ok || len(logic.Args) != 2 {
		t.Fatalf("expr = %#v", expr)
	}
	first := logic.Args[0].(Predicate)
	if first.Args[1] != int64(9007199254740993) {
		t.Fatalf("大整数丢精度了: %#v", first.Args[1])
	}
	second := logic.Args[1].(Predicate)
	if second.Args[1] != float64(1.5) {
		t.Fatalf("小数 = %#v", second.Args[1])
	}
}

// 数组的值/调用歧义由**算符按位置**解决: 这里第三项是值列表, 不是一次调用。
func TestJSONArrayValueVsCall(t *testing.T) {
	expr, err := ParseJSON(`["in", "->categories", [1, 2, 3]]`)
	if err != nil {
		t.Fatal(err)
	}
	predicate, ok := expr.(Predicate)
	if !ok || predicate.Name != "in" {
		t.Fatalf("got %#v", expr)
	}
	values, ok := predicate.Args[1].([]any)
	if !ok || len(values) != 3 || values[0] != int64(1) {
		t.Fatalf("values = %#v", predicate.Args[1])
	}
}

// JSON 里没有占位符: "{:state}" 就是个普通字符串（占位符是 Lisp 专有）。
func TestJSONHasNoPlaceholders(t *testing.T) {
	expr, err := ParseJSON(`["=", "$state", "{:state}"]`)
	if err != nil {
		t.Fatal(err)
	}
	predicate, ok := expr.(Predicate)
	if !ok || predicate.Args[1] != "{:state}" {
		t.Fatalf("got %#v", expr)
	}
}

func TestJSONSyntaxErrors(t *testing.T) {
	cases := []struct{ src, want string }{
		{`{"and": []}`, "condition must be"},
		{`["and"`, "unexpected EOF"},
		{`["and"] []`, "exactly one value"},
		{`[]`, "empty condition"},
		{`["and", 1]`, "condition must be"},
		{`[123, "$a", 1]`, "must be a non-empty string"},
		{``, "EOF"},
	}
	for _, test := range cases {
		_, err := ParseJSON(test.src)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%q: err = %v, want %q", test.src, err, test.want)
		}
	}
}

func TestJSONByteLimit(t *testing.T) {
	src := `["=", "$a", "` + strings.Repeat("x", MaxFilterBytes) + `"]`
	if _, err := ParseJSON(src); err == nil {
		t.Fatal("byte limit must fail")
	}
}
