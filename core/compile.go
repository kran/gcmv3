// 查询编译器: so 的 AST → SQL 条件（dba.Node, 参数绑定交给 dba）。
//
// **纯函数**: 只要 schema（types.Types）, 不碰数据库、不收 ctx、没有副作用。
//
// **一切算符都是注册的** —— 连 `=` `in` `ref` 也是。编译器只有两个 case:
//
//	so.Logic      组合（and / or / not）—— 结构性的, 参数是子表达式
//	so.Predicate  其余一切 —— 查注册表（内建 + 插件, 同一条路）
//
// **片段一律是"模板 + 参数", 不做字符串拼接**: 标识符走 `#{n|quote}`（dba 的
// Quoter 决定方言: sqlite/pg → "x", mysql → `x`）, 值走 `#{n}`, 集合走
// `#{n|expand}`。路径编译成**嵌套的 dba.Node** —— 于是路径自己的占位符编号
// 关在自己的节点里, 算符用 `#{1}` 引用整块, 两边不会撞号。
package core

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kran/dba"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

var (
	ErrInvalidQuery    = errors.New("core: invalid query")
	ErrInvalidField    = errors.New("core: invalid field")
	ErrInvalidOperator = errors.New("core: invalid operator")
	ErrInvalidValue    = errors.New("core: invalid value")
	ErrQueryTooComplex = errors.New("core: query too complex")
)

// maxRelationDepth ref 开层嵌套上限（超过就不是"查数据"而是"写程序"了）。
const maxRelationDepth = 4

// Compiler 把 so 的 AST 编译成 SQL 条件。**无状态, 可并发共用** ——
// 每次编译的计数（节点数/别名）在内部的 compileRun 上。
type Compiler struct {
	types      *types.Types
	predicates map[string]PredicateCompiler
}

// NewCompiler 建编译器并注册标准算符词表。
func NewCompiler(ts *types.Types) *Compiler {
	compiler := &Compiler{types: ts, predicates: map[string]PredicateCompiler{}}
	compiler.registerBuiltins()
	return compiler
}

// RegisterPredicate 注册算符（插件用）: 如 fts 的 match、业务自己的谓词。
// 重名报错 —— 静默覆盖会让"我注册了却没生效"变成最难查的一类问题。
func (c *Compiler) RegisterPredicate(name string, compile PredicateCompiler) error {
	if name == "" {
		return errors.New("core: predicate name required")
	}
	if compile == nil {
		return fmt.Errorf("core: predicate %q: nil compiler", name)
	}
	if _, dup := c.predicates[name]; dup {
		return fmt.Errorf("core: predicate %q already registered", name)
	}
	c.predicates[name] = compile
	return nil
}

// mustRegister 内建算符注册（失败 panic: 程序性错误, 启动就该炸）。
func (c *Compiler) mustRegister(name string, compile PredicateCompiler) {
	err := c.RegisterPredicate(name, compile)
	if err != nil {
		panic(err.Error())
	}
}

// PredicateCompiler 一个算符的编译器: 把参数变成参数绑定的 SQL 片段。
//
// 内建与插件**用完全相同的这个签名** —— 没有特权算符。
type PredicateCompiler func(ctx *PredicateCtx, args []any) (dba.Node, error)

// capability 算符要求 schema 声明的哪种能力（kind.QueryOps / SystemField.Ops）。
// 由每个注册条目**自己声明**, 不按算符名去查第二张表。
type capability uint8

const (
	needEqual capability = iota
	needText
	needOrdered
)

// registerBuiltins 标准算符词表 —— **内建算符只有这一处**。
func (c *Compiler) registerBuiltins() {
	c.mustRegister("=", compare("=", needEqual, true))
	c.mustRegister("!=", compare("!=", needEqual, true))
	c.mustRegister(">", compare(">", needOrdered, false))
	c.mustRegister(">=", compare(">=", needOrdered, false))
	c.mustRegister("<", compare("<", needOrdered, false))
	c.mustRegister("<=", compare("<=", needOrdered, false))
	c.mustRegister("contains", like(false))
	c.mustRegister("prefix", like(true))
	c.mustRegister("in", inPredicate)
	c.mustRegister("exists", existsPredicate(false))
	c.mustRegister("missing", existsPredicate(true))
	c.mustRegister("ref", refPredicate)
	c.mustRegister("true", constantPredicate(true))
	c.mustRegister("false", constantPredicate(false))
}

// Where 编译一个条件（当前行是 nodes 表）。expr 为 nil ⇒ 恒真（不过滤）。
func (c *Compiler) Where(typeName string, expression so.Expr) (dba.Node, error) {
	return c.WhereAt(typeName, expression, "nodes")
}

// WhereAt 同上, 但指定当前行的表别名（ref 开层时用 related_N 别名）。
func (c *Compiler) WhereAt(typeName string, expression so.Expr, nodeRef string) (dba.Node, error) {
	if _, ok := c.types.Type(typeName); !ok {
		return dba.Node{}, fmt.Errorf("%w: type %q not defined", ErrInvalidQuery, typeName)
	}
	if expression == nil {
		return dba.Expr("1 = 1"), nil
	}
	run := &compileRun{Compiler: c}
	return run.expr(expression, typeName, nodeRef, 0)
}

// compileRun 一次编译的状态: 节点数（防炸）与关系别名序号。
type compileRun struct {
	*Compiler
	nodes int
	alias int
}

// expr 编译一个 AST 节点。**只有这两个 case** —— 算符在注册表里。
func (r *compileRun) expr(expression so.Expr, typeName, nodeRef string, depth int) (dba.Node, error) {
	r.nodes++
	if r.nodes > so.MaxFilterNodes || depth > so.MaxFilterDepth {
		return dba.Node{}, ErrQueryTooComplex
	}
	switch expr := expression.(type) {
	case so.Logic:
		return r.compileLogic(expr, typeName, nodeRef, depth)
	case so.Predicate:
		compile, ok := r.predicates[expr.Name]
		if !ok {
			return dba.Node{}, fmt.Errorf("%w: unknown operator %q", ErrInvalidOperator, expr.Name)
		}
		ctx := &PredicateCtx{
			Name:     expr.Name,
			TypeName: typeName,
			NodeRef:  nodeRef,
			run:      r,
			depth:    depth,
		}
		return compile(ctx, expr.Args)
	default:
		return dba.Node{}, fmt.Errorf("%w: unsupported expression %T", ErrInvalidQuery, expression)
	}
}

// exprTree 编译一棵**原始前端树**（算符的参数位置里嵌着子表达式时用, 目前只有 ref）。
func (r *compileRun) exprTree(tree any, typeName, nodeRef string, depth int) (dba.Node, error) {
	expression, err := so.NewReader().Read(tree)
	if err != nil {
		return dba.Node{}, err
	}
	return r.expr(expression, typeName, nodeRef, depth)
}

func (r *compileRun) compileLogic(expr so.Logic, typeName, nodeRef string, depth int) (dba.Node, error) {
	if expr.Op == so.OpNot {
		if len(expr.Args) != 1 {
			return dba.Node{}, fmt.Errorf("%w: not requires one argument", ErrInvalidQuery)
		}
		child, err := r.expr(expr.Args[0], typeName, nodeRef, depth+1)
		if err != nil {
			return dba.Node{}, err
		}
		return dba.Expr("NOT (#{1})", child), nil
	}
	if expr.Op != so.OpAnd && expr.Op != so.OpOr {
		return dba.Node{}, fmt.Errorf("%w: logic operator %q", ErrInvalidOperator, expr.Op)
	}
	if len(expr.Args) == 0 {
		return dba.Node{}, fmt.Errorf("%w: %s requires arguments", ErrInvalidQuery, expr.Op)
	}
	children := make([]dba.Node, len(expr.Args))
	for i := range expr.Args {
		child, err := r.expr(expr.Args[i], typeName, nodeRef, depth+1)
		if err != nil {
			return dba.Node{}, err
		}
		children[i] = child
	}
	separator := " AND "
	if expr.Op == so.OpOr {
		separator = " OR "
	}
	parts := make([]string, len(children))
	args := make([]any, len(children))
	for i := range children {
		parts[i] = fmt.Sprintf("#{%d}", i+1)
		args[i] = children[i]
	}
	return dba.Expr("("+strings.Join(parts, separator)+")", args...), nil
}

// Sort 把排序请求编译成 ORDER BY **片段节点**（不含 "ORDER BY" 这个词）。
//
// 返回 dba.Node 而不是字符串: 标识符因此可以走 `#{n|quote}`（dba 的 Quoter 决定
// 方言）, 与查询片段的其余部分同一套写法 —— 全库没有一处 Go 侧拼标识符了。
//
// 排序能力来自声明（kind.QueryOps.Sortable / SystemField.Ops.Sortable）——
// 不在编译期按名字猜。
func (c *Compiler) Sort(typeName string, fields []so.SortField) (dba.Node, error) {
	if _, ok := c.types.Type(typeName); !ok {
		return dba.Node{}, fmt.Errorf("%w: type %q not defined", ErrInvalidQuery, typeName)
	}
	if len(fields) == 0 {
		return defaultOrder, nil
	}
	if len(fields) > 8 {
		return dba.Node{}, fmt.Errorf("%w: sort exceeds 8 fields", ErrQueryTooComplex)
	}
	parts := make([]dba.Node, 0, len(fields)+1)
	seen := make(map[so.Path]bool, len(fields))
	hasID := false
	for _, sort := range fields {
		if seen[sort.Path] {
			return dba.Node{}, fmt.Errorf("%w: duplicate sort field %q", ErrInvalidField, sort.Path.Field)
		}
		seen[sort.Path] = true
		expression, err := c.sortExpression(typeName, sort.Path)
		if err != nil {
			return dba.Node{}, err
		}
		direction := "ASC"
		if sort.Desc {
			direction = "DESC"
		}
		// 方向是**静态常量**, 直接接在表达式后面; 占位符编号不受影响。
		expression.Text += " " + direction
		parts = append(parts, expression)
		if sort.Path.Kind == so.PathSystem && sort.Path.Field == "id" {
			hasID = true
		}
	}
	// 稳定序: 同值时按 id —— 否则翻页会漏行/重复。
	if !hasID {
		parts = append(parts, idOrder)
	}
	return joinOrder(parts), nil
}

// defaultOrder 列表默认序（最近更新的在前）。
var defaultOrder = dba.Expr("#{1|quote}.#{2|quote} DESC, #{1|quote}.#{3|quote} DESC",
	"nodes", "updated_at", "id")

var idOrder = dba.Expr("#{1|quote}.#{2|quote} DESC", "nodes", "id")

// joinOrder 把若干排序片段接成一个: 各自用 `#{n}` 当参数嵌进来 ——
// **不能直接拼文本**: 每段有自己的占位符编号, 拼起来会撞号。
func joinOrder(parts []dba.Node) dba.Node {
	if len(parts) == 1 {
		return parts[0]
	}
	texts := make([]string, len(parts))
	args := make([]any, len(parts))
	for i, part := range parts {
		texts[i] = fmt.Sprintf("#{%d}", i+1)
		args[i] = part
	}
	return dba.Expr(strings.Join(texts, ", "), args...)
}

// sortExpression 一个可排序路径 → SQL 片段节点（顺带查"能不能排"）。
func (c *Compiler) sortExpression(typeName string, path so.Path) (dba.Node, error) {
	if path.Field == "" {
		return dba.Node{}, fmt.Errorf("%w: empty sort path", ErrInvalidField)
	}
	var (
		expression dba.Node
		sortable   bool
	)
	switch path.Kind {
	case so.PathSystem:
		if !types.IsNodeColumn(path.Field) {
			return dba.Node{}, fmt.Errorf("%w: system field %q", ErrInvalidField, path.Field)
		}
		system, ok := c.types.SystemField(path.Field)
		if !ok {
			return dba.Node{}, fmt.Errorf("%w: system field %q", ErrInvalidField, path.Field)
		}
		sortable = system.Ops.Sortable
		expression = dba.Expr("#{1|quote}.#{2|quote}", "nodes", path.Field)
	case so.PathField:
		field, ok := c.types.Field(typeName, path.Field)
		if !ok || c.types.IsRefKind(field.Kind) {
			return dba.Node{}, fmt.Errorf("%w: %s.%s", ErrInvalidField, typeName, path.Field)
		}
		sortable = c.types.FieldQueryOps(field).Sortable
		// 字段名进的是**值**（JSON 路径字符串）, 不是标识符
		expression = dba.Expr("json_extract(#{1|quote}.fields, #{2})", "nodes", "$."+field.Name)
	default:
		// 关系不能排序: 它是多值/需要 EXISTS, 不是一个可比的值。
		return dba.Node{}, fmt.Errorf("%w: relation %q cannot be sorted", ErrInvalidField, path.Field)
	}
	if !sortable {
		return dba.Node{}, fmt.Errorf("%w: field %q cannot be sorted", ErrInvalidField, path.Field)
	}
	return expression, nil
}

// ── 算符拿到的上下文 ────────────────────────────

// PredicateCtx 算符编译拿到的结构化上下文。内建与插件看到的是同一个东西。
type PredicateCtx struct {
	// Name 算符名（"="/"in"/"match"…）。
	Name string
	// TypeName 当前行所属类型。
	TypeName string
	// NodeRef 当前行的表别名（"nodes", 或 ref 开层后的 "related_1"）。
	NodeRef string

	run   *compileRun
	depth int
}

// Path 解析一个路径 token（"$field" / "id" / "->ref" / "<-type.field"）→
// 可直接嵌进片段的 dba.Node（自己带参数编号）。字段不存在 ⇒ 报错。
//
// 这是**唯一的字段取用口**: 算符绕不过 schema 白名单。
func (c *PredicateCtx) Path(token any) (dba.Node, error) {
	resolved, err := c.resolve(token)
	if err != nil {
		return dba.Node{}, err
	}
	if !resolved.scalar() {
		return dba.Node{}, fmt.Errorf("%w: %v is a relation, not a value", ErrInvalidField, token)
	}
	return resolved.node, nil
}

// Values 解析一个集合位置的参数: 数组 → 逐项; 单值 → 单元素。
// 这里是集合大小的封顶处（前端不可信）。
func (c *PredicateCtx) Values(arg any) ([]any, error) {
	if arg == nil {
		return nil, fmt.Errorf("%w: collection required, got null", ErrInvalidValue)
	}
	values, ok := arg.([]any)
	if !ok {
		return []any{arg}, nil
	}
	if len(values) > so.MaxSetValues {
		return nil, fmt.Errorf("%w: collection exceeds %d items", ErrQueryTooComplex, so.MaxSetValues)
	}
	return values, nil
}

// Expr 把一个**原始子表达式**（前端树）编译进来 —— 参数位置里嵌条件时用。
func (c *PredicateCtx) Expr(arg any) (dba.Node, error) {
	return c.run.exprTree(arg, c.TypeName, c.NodeRef, c.depth+1)
}

// Field 当前类型的字段定义（算符要自己判断能力时用）。
func (c *PredicateCtx) Field(name string) (types.FieldDef, bool) {
	return c.run.types.Field(c.TypeName, name)
}

// resolve 解析路径 token → 完整解析结果（路径 + 类型信息 + 可直接嵌的 SQL 节点）。
func (c *PredicateCtx) resolve(token any) (resolvedPath, error) {
	path, err := so.ParsePath(token)
	if err != nil {
		return resolvedPath{}, err
	}
	return c.run.resolvePath(path, c.TypeName, c.NodeRef)
}

// checkCapability 只要能力 —— 能力来自 schema 声明（kind.QueryOps / SystemField.Ops）。
// 与值校验分开: null 值不需要值校验, 但仍然要过能力。
func (c *PredicateCtx) checkCapability(resolved resolvedPath, need capability) error {
	if resolved.hasField {
		return checkCapability(resolved.fieldName, c.run.types.FieldQueryOps(resolved.field), need)
	}
	system, ok := c.run.types.SystemField(resolved.fieldName)
	if !ok {
		return fmt.Errorf("%w: system field %q", ErrInvalidField, resolved.fieldName)
	}
	return checkCapability(resolved.fieldName, system.Ops, need)
}

// checkValue 能力 + 值类型（后者由 kind 自己的 Validate 决定）。
func (c *PredicateCtx) checkValue(resolved resolvedPath, value any, need capability) error {
	err := c.checkCapability(resolved, need)
	if err != nil {
		return err
	}
	if resolved.hasField {
		err := c.run.types.ValidateValue(c.TypeName, resolved.field, value)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrInvalidValue, resolved.fieldName, err)
		}
		return nil
	}
	system, _ := c.run.types.SystemField(resolved.fieldName)
	err = system.Validate(value)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalidValue, resolved.fieldName, err)
	}
	return nil
}

// checkCapability 算符要求的能力 vs schema 声明的能力。
func checkCapability(field string, ops types.QueryOps, need capability) error {
	var ok bool
	var what string
	switch need {
	case needEqual:
		ok, what = ops.Equal, "equality"
	case needText:
		ok, what = ops.Text, "text search"
	case needOrdered:
		ok, what = ops.Ordered, "ordering"
	}
	if !ok {
		return fmt.Errorf("%w: %s does not support %s", ErrInvalidOperator, field, what)
	}
	return nil
}

// resolvedPath 一条路径解析出来的东西。
type resolvedPath struct {
	path      so.Path
	node      dba.Node // 标量/列的 SQL（关系路径时为空）
	field     types.FieldDef
	hasField  bool
	target    string // 关系目标类型（ref 开层用）
	fieldName string
}

// scalar 是不是标量值（关系路径不是 —— 它得走 EXISTS）。
func (r resolvedPath) scalar() bool { return r.node.Text != "" }

func (c *Compiler) resolvePath(path so.Path, typeName, nodeRef string) (resolvedPath, error) {
	if path.Field == "" {
		return resolvedPath{}, fmt.Errorf("%w: empty path", ErrInvalidField)
	}
	switch path.Kind {
	case so.PathSystem:
		if !types.IsNodeColumn(path.Field) {
			return resolvedPath{}, fmt.Errorf("%w: system field %q", ErrInvalidField, path.Field)
		}
		return resolvedPath{
			path:      path,
			node:      dba.Expr("#{1|quote}.#{2|quote}", nodeRef, path.Field),
			fieldName: path.Field,
		}, nil
	case so.PathField:
		field, ok := c.types.Field(typeName, path.Field)
		if !ok || c.types.IsRefKind(field.Kind) {
			return resolvedPath{}, fmt.Errorf("%w: %s.%s", ErrInvalidField, typeName, path.Field)
		}
		return resolvedPath{
			path:      path,
			node:      dba.Expr("json_extract(#{1|quote}.fields, #{2})", nodeRef, "$."+field.Name),
			field:     field,
			hasField:  true,
			fieldName: field.Name,
		}, nil
	case so.PathOutRef:
		field, ok := c.types.Field(typeName, path.Field)
		if !ok || !c.types.IsRefKind(field.Kind) {
			return resolvedPath{}, fmt.Errorf("%w: %s.%s is not a ref", ErrInvalidField, typeName, path.Field)
		}
		return resolvedPath{path: path, field: field, hasField: true, target: field.To, fieldName: field.Name}, nil
	case so.PathInRef:
		if path.SourceType == "" {
			return resolvedPath{}, fmt.Errorf("%w: incoming source type required", ErrInvalidField)
		}
		field, ok := c.types.Field(path.SourceType, path.Field)
		if !ok || !c.types.IsRefKind(field.Kind) || field.To != typeName {
			return resolvedPath{}, fmt.Errorf("%w: incoming %s.%s does not target %s",
				ErrInvalidField, path.SourceType, path.Field, typeName)
		}
		return resolvedPath{path: path, field: field, hasField: true, target: path.SourceType, fieldName: field.Name}, nil
	default:
		return resolvedPath{}, fmt.Errorf("%w: unknown path kind", ErrInvalidField)
	}
}

// incoming 是入边路径。
func (r resolvedPath) incoming() bool { return r.path.Kind == so.PathInRef }

// ── 内建算符 ────────────────────────────────────

// arity 参数个数（JSON/树形态没有键名可言 ⇒ 个数只能逐算符查）。
func arity(name string, args []any, want int) error {
	if len(args) != want {
		return fmt.Errorf("%w: %s takes %d argument(s), got %d", ErrInvalidOperator, name, want, len(args))
	}
	return nil
}

// compare 比较算符: 参数 (路径, 值)。sign 直接就是 SQL 运算符;
// needs 声明它要求的能力; nullOK 表示 null 值走 IS [NOT] NULL。
func compare(sign string, needs capability, nullOK bool) PredicateCompiler {
	return func(ctx *PredicateCtx, args []any) (dba.Node, error) {
		err := arity(ctx.Name, args, 2)
		if err != nil {
			return dba.Node{}, err
		}
		resolved, err := ctx.resolve(args[0])
		if err != nil {
			return dba.Node{}, err
		}
		if !resolved.scalar() {
			return dba.Node{}, fmt.Errorf("%w: %v needs in/exists/ref, not %s",
				ErrInvalidOperator, args[0], ctx.Name)
		}
		value := args[1]
		if value == nil {
			if !nullOK {
				return dba.Node{}, fmt.Errorf("%w: null only supports = and !=", ErrInvalidValue)
			}
			// null 只过能力: 它没有"值的类型"可校验
			err = ctx.checkCapability(resolved, needs)
			if err != nil {
				return dba.Node{}, err
			}
			if sign == "=" {
				return dba.Expr("#{1} IS NULL", resolved.node), nil
			}
			return dba.Expr("#{1} IS NOT NULL", resolved.node), nil
		}
		err = ctx.checkValue(resolved, value, needs)
		if err != nil {
			return dba.Node{}, err
		}
		return dba.Expr("#{1} "+sign+" #{2}", resolved.node, value), nil
	}
}

// like contains / prefix: 走 LIKE, 用户输入里的 % _ \ 必须转义。
func like(prefix bool) PredicateCompiler {
	return func(ctx *PredicateCtx, args []any) (dba.Node, error) {
		err := arity(ctx.Name, args, 2)
		if err != nil {
			return dba.Node{}, err
		}
		resolved, err := ctx.resolve(args[0])
		if err != nil {
			return dba.Node{}, err
		}
		text, ok := args[1].(string)
		if !ok {
			return dba.Node{}, fmt.Errorf("%w: %s requires a string, got %T", ErrInvalidValue, ctx.Name, args[1])
		}
		err = ctx.checkValue(resolved, text, needText)
		if err != nil {
			return dba.Node{}, err
		}
		pattern := escapeLike(text) + "%"
		if !prefix {
			pattern = "%" + escapeLike(text) + "%"
		}
		return dba.Expr(`#{1} LIKE #{2} ESCAPE '\'`, resolved.node, pattern), nil
	}
}

// inPredicate 集合: 标量字段走 IN; 引用字段走**边表驱动**的子查询
// （非相关 EXISTS —— 相关子查询会对每个候选行探一次边, 大表上是 O(候选行数)）。
func inPredicate(ctx *PredicateCtx, args []any) (dba.Node, error) {
	err := arity(ctx.Name, args, 2)
	if err != nil {
		return dba.Node{}, err
	}
	resolved, err := ctx.resolve(args[0])
	if err != nil {
		return dba.Node{}, err
	}
	values, err := ctx.Values(args[1])
	if err != nil {
		return dba.Node{}, err
	}
	if len(values) == 0 {
		return dba.Expr("1 = 0"), nil
	}
	if resolved.scalar() {
		for _, value := range values {
			err = ctx.checkValue(resolved, value, needEqual)
			if err != nil {
				return dba.Node{}, err
			}
		}
		return dba.Expr("#{1} IN (#{2|expand})", resolved.node, values), nil
	}

	if err := validateIDs(values); err != nil {
		return dba.Node{}, err
	}
	nodeRef, field := ctx.NodeRef, resolved.fieldName
	if resolved.incoming() {
		return dba.Expr(`#{1|quote}.id IN (SELECT e.to_node FROM edges e JOIN nodes src
			ON src.id = e.from_node WHERE e.field = #{2} AND src.type = #{3}
			 AND e.from_node IN (#{4|expand}))`, nodeRef, field, resolved.path.SourceType, values), nil
	}
	return dba.Expr(`#{1|quote}.id IN (SELECT e.from_node FROM edges e
		WHERE e.field = #{2} AND e.to_node IN (#{3|expand}))`, nodeRef, field, values), nil
}

// existsPredicate 存在性（missing 取反）。
//
// 参数编号统一: `#{1|quote}` = 当前行别名, `#{2}` = 字段名, `#{3}` = 来源类型。
func existsPredicate(missing bool) PredicateCompiler {
	return func(ctx *PredicateCtx, args []any) (dba.Node, error) {
		err := arity(ctx.Name, args, 1)
		if err != nil {
			return dba.Node{}, err
		}
		resolved, err := ctx.resolve(args[0])
		if err != nil {
			return dba.Node{}, err
		}
		nodeRef, field := ctx.NodeRef, resolved.fieldName
		var node dba.Node
		switch resolved.path.Kind {
		case so.PathSystem:
			node = dba.Expr("#{1} IS NOT NULL", resolved.node)
		case so.PathField:
			node = dba.Expr("json_type(#{1|quote}.fields, #{2}) IS NOT NULL", nodeRef, "$."+field)
		case so.PathInRef:
			// 来源要按类型筛（同名字段可能出现在别的类型上）
			node = dba.Expr(`EXISTS(SELECT 1 FROM edges e JOIN nodes src ON src.id = e.from_node
				WHERE e.field = #{2} AND e.to_node = #{1|quote}.id AND src.type = #{3})`,
				nodeRef, field, resolved.path.SourceType)
		default:
			node = dba.Expr(`EXISTS(SELECT 1 FROM edges e
				WHERE e.field = #{2} AND e.from_node = #{1|quote}.id)`, nodeRef, field)
		}
		if missing {
			return dba.Expr("NOT (#{1})", node), nil
		}
		return node, nil
	}
}

// refPredicate 打开一个关系: 参数 (关系路径, 子表达式)。子表达式在新别名下编译,
// 且校验的是**目标类型** —— 所以 `ref ->categories (= $title "x")` 会在
// category 上找 title 并报错。
//
// 参数编号统一: `#{1|quote}` = 本层的别名（子表达式用的就是它）, `#{2|quote}` = 当前行。
func refPredicate(ctx *PredicateCtx, args []any) (dba.Node, error) {
	err := arity(ctx.Name, args, 2)
	if err != nil {
		return dba.Node{}, err
	}
	resolved, err := ctx.resolve(args[0])
	if err != nil {
		return dba.Node{}, err
	}
	if resolved.path.Kind != so.PathOutRef && resolved.path.Kind != so.PathInRef {
		return dba.Node{}, fmt.Errorf("%w: ref requires a relation path, got %v", ErrInvalidOperator, args[0])
	}
	if ctx.depth+1 >= maxRelationDepth {
		return dba.Node{}, fmt.Errorf("%w: ref depth exceeds %d", ErrQueryTooComplex, maxRelationDepth)
	}
	run := ctx.run
	run.alias++
	alias := fmt.Sprintf("related_%d", run.alias)
	child, err := run.exprTree(args[1], resolved.target, alias, ctx.depth+1)
	if err != nil {
		return dba.Node{}, err
	}
	nodeRef, field := ctx.NodeRef, resolved.fieldName
	if resolved.incoming() {
		return dba.Expr(`EXISTS(SELECT 1 FROM edges e JOIN nodes #{1|quote}
			ON #{1|quote}.id = e.from_node WHERE e.to_node = #{2|quote}.id
			 AND e.field = #{3} AND #{1|quote}.type = #{4} AND #{5})`,
			alias, nodeRef, field, resolved.path.SourceType, child), nil
	}
	return dba.Expr(`EXISTS(SELECT 1 FROM edges e JOIN nodes #{1|quote}
		ON #{1|quote}.id = e.to_node WHERE e.from_node = #{2|quote}.id
		 AND e.field = #{3} AND #{4})`, alias, nodeRef, field, child), nil
}

// constantPredicate 恒真 / 恒假。
func constantPredicate(value bool) PredicateCompiler {
	return func(ctx *PredicateCtx, args []any) (dba.Node, error) {
		err := arity(ctx.Name, args, 0)
		if err != nil {
			return dba.Node{}, err
		}
		if value {
			return dba.Expr("1 = 1"), nil
		}
		return dba.Expr("1 = 0"), nil
	}
}

func validateIDs(values []any) error {
	for _, value := range values {
		id, err := types.ToID(value)
		if err != nil || id <= 0 {
			return fmt.Errorf("%w: relation values must be positive node IDs", ErrInvalidValue)
		}
	}
	return nil
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}
