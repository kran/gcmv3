package core

import (
	"errors"
	"strings"
	"testing"

	"github.com/kran/dba"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// 编译器是纯的: 只要 schema。**连数据库句柄都不需要** —— dba.Node 就是
// {Text, Args}, 断言它比断言渲染出来的字符串更准（渲染是 dba 的事）。
const compileTypesYAML = `
types:
  category:
    fields:
      - { name: name, kind: text }
      - { name: parent, kind: ref, to: category, transitive: true }
  person:
    fields:
      - { name: name, kind: text }
      - { name: featured, kind: bool }
      - { name: mentor, kind: ref, to: person }
  article:
    fields:
      - { name: title, kind: text }
      - { name: featured, kind: bool }
      - { name: views, kind: number }
      - { name: state, kind: select, options: [draft, published] }
      - { name: published_at, kind: timestamp }
      - { name: categories, kind: "refs", to: category }
      - { name: authors, kind: "refs", to: person }
`

func compileFixture(t *testing.T) *Compiler {
	t.Helper()
	ts := types.New()
	err := ts.Load([]byte(compileTypesYAML))
	if err != nil {
		t.Fatal(err)
	}
	return NewCompiler(ts)
}

// compileWhere 走完整链路: 前端树 →(so.Reader)→ AST →(编译器)→ SQL。
// 这正是运行时的链路, 所以测试顺带把两段都盖住了。
func compileWhere(t *testing.T, c *Compiler, typeName string, where so.Where) (string, []any, error) {
	t.Helper()
	expr, err := where.Expr()
	if err != nil {
		return "", nil, err
	}
	return compileExpr(c, typeName, expr)
}

func compileExpr(c *Compiler, typeName string, expr so.Expr) (string, []any, error) {
	node, err := c.Where(typeName, expr)
	return node.Text, node.Args, err
}

func mustCompile(t *testing.T, c *Compiler, typeName string, where so.Where) (string, []any) {
	t.Helper()
	text, args, err := compileWhere(t, c, typeName, where)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return text, args
}

func mustFail(t *testing.T, c *Compiler, typeName string, where so.Where, want error) {
	t.Helper()
	_, _, err := compileWhere(t, c, typeName, where)
	assertError(t, err, want)
}

func mustFailExpr(t *testing.T, c *Compiler, typeName string, expr so.Expr, want error) {
	t.Helper()
	_, _, err := compileExpr(c, typeName, expr)
	assertError(t, err, want)
}

func assertError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// child 取嵌套片段（组合算符的参数就是 dba.Node）。
func child(t *testing.T, args []any, i int) dba.Node {
	t.Helper()
	node, ok := args[i].(dba.Node)
	if !ok {
		t.Fatalf("args[%d] = %#v, want dba.Node", i, args[i])
	}
	return node
}

// pathNode 取路径节点（路径编译成嵌套的 dba.Node —— 它自己的占位符编号
// 关在自己里面, 算符用 #{1} 引用整块）。
func pathNode(t *testing.T, args []any, i int) (string, []any) {
	t.Helper()
	node := child(t, args, i)
	return node.Text, node.Args
}

// slice 取 #{n|expand} 的参数。
func slice(t *testing.T, args []any, i int) []any {
	t.Helper()
	values, ok := args[i].([]any)
	if !ok {
		t.Fatalf("args[%d] = %#v, want []any", i, args[i])
	}
	return values
}

// ── 标量 ─────────────────────────────────────

// 类型字段 → json_extract; 系统列 → 列名; 值一律走参数绑定。
func TestCompileScalar(t *testing.T) {
	c := compileFixture(t)

	text, args := mustCompile(t, c, "article", so.P("=", "$title", "甲"))
	if text != "#{1} = #{2}" {
		t.Fatalf("text = %s", text)
	}
	pathText, pathArgs := pathNode(t, args, 0)
	if pathText != "json_extract(#{1|quote}.fields, #{2})" {
		t.Fatalf("字段节点 = %s", pathText)
	}
	if len(pathArgs) != 2 || pathArgs[0] != "nodes" || pathArgs[1] != "$.title" {
		t.Fatalf("字段参数 = %#v", pathArgs)
	}
	if args[1] != "甲" {
		t.Fatalf("args = %#v", args)
	}

	// 系统列: 别名与列名都走 dba 的 quote（Quoter 决定方言）
	_, args = mustCompile(t, c, "article", so.P("=", "id", int64(7)))
	pathText, pathArgs = pathNode(t, args, 0)
	if pathText != "#{1|quote}.#{2|quote}" || pathArgs[1] != "id" {
		t.Fatalf("系统列节点 = %s %#v", pathText, pathArgs)
	}
	if args[1] != int64(7) {
		t.Fatalf("args = %#v", args)
	}

	text, _ = mustCompile(t, c, "article", so.P(">=", "$views", int64(10)))
	if text != "#{1} >= #{2}" {
		t.Fatalf("text = %s", text)
	}
}

// LIKE 的转义: % _ \ 都要吃掉, 否则用户输入里的 % 会变成通配符。
func TestCompileLikeEscaping(t *testing.T) {
	c := compileFixture(t)
	text, args := mustCompile(t, c, "article", so.P("contains", "$title", `50%_off\`))
	if text != `#{1} LIKE #{2} ESCAPE '\'` {
		t.Fatalf("text = %s", text)
	}
	if args[1] != `%50\%\_off\\%` {
		t.Fatalf("escape = %#v", args[1])
	}
	_, args = mustCompile(t, c, "article", so.P("prefix", "$title", "A_"))
	if args[1] != `A\_%` {
		t.Fatalf("prefix escape = %#v", args[1])
	}
}

// 空集合 ⇒ 恒假; 超上限 ⇒ 报错（都不静默）。
func TestCompileInBounds(t *testing.T) {
	c := compileFixture(t)
	text, _ := mustCompile(t, c, "article", so.P("in", "$state", []any{}))
	if text != "1 = 0" {
		t.Fatalf("空集合必须恒假: %s", text)
	}
	values := make([]any, so.MaxSetValues+1)
	for i := range values {
		values[i] = "draft"
	}
	mustFail(t, c, "article", so.P("in", "$state", values), ErrQueryTooComplex)
}

// 能力校验来自 schema 声明（kind.QueryOps / SystemField.Ops）, 不按名字猜。
func TestCompileCapabilityChecks(t *testing.T) {
	c := compileFixture(t)
	cases := []struct {
		name  string
		where so.Where
		want  error
	}{
		{"text 不能比大小", so.P(">", "$title", "x"), ErrInvalidOperator},
		{"number 不能 contains", so.P("contains", "$views", "1"), ErrInvalidOperator},
		{"bool 不能比大小", so.P(">", "$featured", true), ErrInvalidOperator},
		{"bool 不能 contains", so.P("contains", "$featured", "x"), ErrInvalidOperator},
		{"select 值必须合法", so.P("=", "$state", "unknown"), ErrInvalidValue},
		{"number 不能收字符串", so.P("=", "$views", "十"), ErrInvalidValue},
		{"不存在的字段", so.P("=", "$missing", "x"), ErrInvalidField},
		{"不存在的系统列", so.P("=", "nonexistent", "x"), ErrInvalidField},
		{"关系字段不能直接比较", so.P("=", "->categories", int64(1)), ErrInvalidOperator},
		{"关系字段不能 contains", so.P("contains", "->categories", "x"), ErrInvalidOperator},
		{"非 =/!= 不收 null", so.P(">", "$views", nil), ErrInvalidValue},
		{"contains 要字符串", so.P("contains", "$title", 42), ErrInvalidValue},
		{"参数个数不对", so.P("=", "$title"), ErrInvalidOperator},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mustFail(t, c, "article", test.where, test.want)
		})
	}
}

// NULL 语义: (= $x null) → IS NULL; (!= $x null) → IS NOT NULL。
func TestCompileNull(t *testing.T) {
	c := compileFixture(t)
	text, _ := mustCompile(t, c, "article", so.P("=", "$published_at", nil))
	if text != "#{1} IS NULL" {
		t.Fatalf("text = %s", text)
	}
	text, _ = mustCompile(t, c, "article", so.P("!=", "$published_at", nil))
	if text != "#{1} IS NOT NULL" {
		t.Fatalf("text = %s", text)
	}
}

// ── 关系 ─────────────────────────────────────

// 引用: 边表驱动（不是相关 EXISTS）, 对称关系双向。
func TestCompileRefIn(t *testing.T) {
	c := compileFixture(t)
	text, args := mustCompile(t, c, "article", so.P("in", "->categories", []any{int64(1), int64(2)}))
	if !strings.Contains(text, "#{1|quote}.id IN") || !strings.Contains(text, "e.to_node IN (#{3|expand})") {
		t.Fatalf("text = %s", text)
	}
	if args[0] != "nodes" || args[1] != "categories" {
		t.Fatalf("args = %#v", args)
	}
	if values := slice(t, args, 2); len(values) != 2 || values[0] != int64(1) {
		t.Fatalf("values = %#v", values)
	}

	// 入边: 来源类型进参数
	text, args = mustCompile(t, c, "category", so.P("in", "<-article.categories", []any{int64(3)}))
	if !strings.Contains(text, "JOIN nodes src") {
		t.Fatalf("text = %s", text)
	}
	if args[0] != "nodes" || args[1] != "categories" || args[2] != "article" {
		t.Fatalf("args = %#v", args)
	}

	// 引用值必须是正整数 id
	mustFail(t, c, "article", so.P("in", "->categories", []any{"abc"}), ErrInvalidValue)
}

// exists / missing。
func TestCompileExists(t *testing.T) {
	c := compileFixture(t)
	text, args := mustCompile(t, c, "article", so.P("exists", "$title"))
	if text != "json_type(#{1|quote}.fields, #{2}) IS NOT NULL" {
		t.Fatalf("text = %s", text)
	}
	if len(args) != 2 || args[1] != "$.title" {
		t.Fatalf("args = %#v", args)
	}

	text, args = mustCompile(t, c, "article", so.P("missing", "->categories"))
	if text != "NOT (#{1})" {
		t.Fatalf("missing 必须取反: %s", text)
	}
	if !strings.Contains(child(t, args, 0).Text, "EXISTS(") {
		t.Fatalf("子片段 = %s", child(t, args, 0).Text)
	}
}

// ref 开层: 别名递增 + 目标类型换掉 + 深度封顶。
func TestCompileRefDepth(t *testing.T) {
	c := compileFixture(t)
	text, args := mustCompile(t, c, "article",
		so.REF("->categories", so.P("=", "$name", "新闻")))
	if !strings.Contains(text, "JOIN nodes #{1|quote}") {
		t.Fatalf("text = %s", text)
	}
	if args[0] != "related_1" || args[2] != "categories" {
		t.Fatalf("别名/字段 = %#v", args)
	}
	childText, childArgs := pathNode(t, child(t, args, 3).Args, 0)
	if childText != "json_extract(#{1|quote}.fields, #{2})" || childArgs[0] != "related_1" {
		t.Fatalf("开层后必须用新别名: %s %#v", childText, childArgs)
	}

	// 开层后字段要在**目标类型**上校验: category 没有 title
	mustFail(t, c, "article", so.REF("->categories", so.P("=", "$title", "x")), ErrInvalidField)

	// 递归 category.parent 到超过上限
	deep := so.P("=", "$name", "x")
	for i := 0; i <= maxRelationDepth; i++ {
		deep = so.REF("->parent", deep)
	}
	mustFail(t, c, "category", deep, ErrQueryTooComplex)

	// ref 不接标量路径
	mustFail(t, c, "article", so.REF("$title", so.P("=", "$name", "x")), ErrInvalidOperator)
	// 子表达式缺失也要报错（arity）
	mustFail(t, c, "article", so.P("ref", "->categories"), ErrInvalidOperator)
}

// ── 逻辑与常量 ────────────────────────────────

// 逻辑: 子片段按占位符嵌套（组合算符的参数就是 dba.Node）。
func TestCompileLogic(t *testing.T) {
	c := compileFixture(t)
	text, args := mustCompile(t, c, "article", so.AND(
		so.P("=", "$state", "published"),
		so.OR(
			so.P(">", "$views", int64(100)),
			so.NOT(so.P("=", "$title", "草稿")),
		),
	))
	if text != "(#{1} AND #{2})" {
		t.Fatalf("text = %s", text)
	}
	nested := child(t, args, 1)
	if nested.Text != "(#{1} OR #{2})" {
		t.Fatalf("嵌套 text = %s", nested.Text)
	}
	notNode := child(t, nested.Args, 1)
	if notNode.Text != "NOT (#{1})" {
		t.Fatalf("not text = %s", notNode.Text)
	}
}

// 非法形状直接构造 AST 也要被挡住（不是只靠构造器约束）。
func TestCompileLogicShapes(t *testing.T) {
	c := compileFixture(t)
	mustFailExpr(t, c, "article", so.Logic{Op: so.OpNot, Args: nil}, ErrInvalidQuery)
	mustFailExpr(t, c, "article", so.Logic{Op: so.OpAnd}, ErrInvalidQuery)
	mustFailExpr(t, c, "article", so.Logic{Op: "xor", Args: []so.Expr{so.Predicate{Name: "true"}}}, ErrInvalidOperator)
}

// 恒真 / 恒假 与 nil 条件。
func TestCompileConstants(t *testing.T) {
	c := compileFixture(t)
	text, _ := mustCompile(t, c, "article", so.P("true"))
	if text != "1 = 1" {
		t.Fatalf("text = %s", text)
	}
	text, _ = mustCompile(t, c, "article", so.P("false"))
	if text != "1 = 0" {
		t.Fatalf("text = %s", text)
	}
	text, _, _ = compileExpr(c, "article", nil)
	if text != "1 = 1" {
		t.Fatalf("nil 条件必须恒真: %s", text)
	}
}

// 类型必须先定义。
func TestCompileUnknownType(t *testing.T) {
	c := compileFixture(t)
	mustFail(t, c, "ghost", so.P("=", "$a", 1), ErrInvalidQuery)
}

// 未知算符 ⇒ 报错（lisp/JSON 里手写未知算符的落点）。
func TestCompileUnknownOperator(t *testing.T) {
	c := compileFixture(t)
	_, _, err := compileWhere(t, c, "article", so.P("match", "词"))
	if !errors.Is(err, ErrInvalidOperator) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "match") {
		t.Fatalf("错误信息要点出算符名: %v", err)
	}
}

// 复杂度上限: 节点数。
func TestCompileNodeLimit(t *testing.T) {
	c := compileFixture(t)
	args := make([]so.Expr, 0, so.MaxFilterNodes+1)
	for i := 0; i <= so.MaxFilterNodes; i++ {
		args = append(args, so.Predicate{Name: "true"})
	}
	mustFailExpr(t, c, "article", so.Logic{Op: so.OpAnd, Args: args}, ErrQueryTooComplex)
}

// ── 算符注册（内建与插件同一条路）────────────────

func TestPredicateRegistration(t *testing.T) {
	c := compileFixture(t)
	if err := c.RegisterPredicate("", echoPredicate); err == nil {
		t.Fatal("空名字必须报错")
	}
	if err := c.RegisterPredicate("match", nil); err == nil {
		t.Fatal("nil 编译器必须报错")
	}
	if err := c.RegisterPredicate("=", echoPredicate); err == nil {
		t.Fatal("重名（连内建也算）必须报错")
	}
	if err := c.RegisterPredicate("match", echoPredicate); err != nil {
		t.Fatal(err)
	}

	text, args := mustCompile(t, c, "article", so.P("match", "人工智能"))
	if text != "#{1} IS NOT NULL AND nodes_fts MATCH #{2}" {
		t.Fatalf("text = %s", text)
	}
	pathText, pathArgs := pathNode(t, args, 0)
	if pathText != "json_extract(#{1|quote}.fields, #{2})" || pathArgs[1] != "$.title" {
		t.Fatalf("字段节点 = %s %#v", pathText, pathArgs)
	}
	if args[1] != "人工智能" {
		t.Fatalf("args = %#v", args)
	}
}

// echoPredicate 演示插件算符能做什么: 用 ctx.Path 拿字段节点（所以绕不过白名单）,
// 并把它当成自己片段里的一项（#{1}）—— 编号关在各自节点里, 不会撞。
func echoPredicate(ctx *PredicateCtx, args []any) (dba.Node, error) {
	column, err := ctx.Path("$title")
	if err != nil {
		return dba.Node{}, err
	}
	return dba.Expr("#{1} IS NOT NULL AND nodes_fts MATCH #{2}", column, args[0]), nil
}

// 插件算符里的字段同样过白名单 —— 它绕不过 schema。
func TestPredicateFieldWhitelist(t *testing.T) {
	c := compileFixture(t)
	err := c.RegisterPredicate("probe", func(ctx *PredicateCtx, args []any) (dba.Node, error) {
		_, err := ctx.Path("$ghost")
		return dba.Node{}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	mustFail(t, c, "article", so.P("probe"), ErrInvalidField)

	// 关系字段拿不到 SQL（它不是标量值）
	err = c.RegisterPredicate("rel", func(ctx *PredicateCtx, args []any) (dba.Node, error) {
		_, err := ctx.Path("->categories")
		return dba.Node{}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	mustFail(t, c, "article", so.P("rel"), ErrInvalidField)
}

// 插件算符也能在开层里用, 并且拿到开层后的上下文。
func TestPredicateInsideRef(t *testing.T) {
	c := compileFixture(t)
	err := c.RegisterPredicate("seen", func(ctx *PredicateCtx, args []any) (dba.Node, error) {
		if ctx.NodeRef != "related_1" || ctx.TypeName != "category" {
			return dba.Node{}, errors.New("ctx = " + ctx.NodeRef + "/" + ctx.TypeName)
		}
		return dba.Expr("1 = 1"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = compileWhere(t, c, "article", so.REF("->categories", so.P("seen")))
	if err != nil {
		t.Fatalf("算符在 ref 里: %v", err)
	}
}

// 插件算符可以用 ctx.Expr 嵌子条件（与内建的组合能力一致）。
func TestPredicateNestsSubExpression(t *testing.T) {
	c := compileFixture(t)
	err := c.RegisterPredicate("either", func(ctx *PredicateCtx, args []any) (dba.Node, error) {
		left, err := ctx.Expr(args[0])
		if err != nil {
			return dba.Node{}, err
		}
		right, err := ctx.Expr(args[1])
		if err != nil {
			return dba.Node{}, err
		}
		return dba.Expr("(#{1} OR #{2})", left, right), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	text, args := mustCompile(t, c, "article",
		so.P("either", []any{"=", "$state", "draft"}, []any{"=", "$title", "x"}))
	if text != "(#{1} OR #{2})" {
		t.Fatalf("text = %s", text)
	}
	left := child(t, args, 0)
	if left.Text != "#{1} = #{2}" {
		t.Fatalf("左 = %s", left.Text)
	}
	leftPath, leftArgs := pathNode(t, left.Args, 0)
	if leftArgs[1] != "$.state" || leftPath != "json_extract(#{1|quote}.fields, #{2})" {
		t.Fatalf("左路径 = %s %#v", leftPath, leftArgs)
	}
}

// 编译器无状态: 别名每次从 1 开始（可并发共用同一个 *Compiler）。
func TestCompilerReusable(t *testing.T) {
	c := compileFixture(t)
	_, firstArgs := mustCompile(t, c, "article", so.REF("->categories", so.P("=", "$name", "a")))
	_, secondArgs := mustCompile(t, c, "article", so.REF("->categories", so.P("=", "$name", "b")))
	if firstArgs[0] != "related_1" || secondArgs[0] != "related_1" {
		t.Fatalf("别名必须每次从 1 开始: %#v %#v", firstArgs[0], secondArgs[0])
	}
}
