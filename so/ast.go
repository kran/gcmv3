// Package so 查询语言: 前端树 + 两种文本写法（Lisp / JSON）+ 编译期用的 AST。
//
// 三种写法产出**同一棵树**, 树由 Reader 读成 AST:
//
//	so.AND(so.P("=", "$state", "published"))      Go 构造器
//	(and (= $state "published"))                  Lisp
//	["and", ["=", "$state", "published"]]         JSON
//
// **一切算符都是注册的**: 语言没有特权算符 —— `=`, `in`, `ref`, 插件加的
// `match`, 在 core 的编译器眼里都是"一个名字 + 一组参数"。所以这里的 AST
// 只有两种节点:
//
//	Logic      组合（and / or / not）—— 结构上就是"含子表达式的节点"
//	Predicate  其余一切 —— 名字由 core 的算符注册表决定
//
// 校验（算符认不认、参数几个、路径合不合法）全在 core 的编译器里, 因为
// 算符词表在那边。这里只负责**结构**: 数组=调用, and/or/not 的子项是子表达式。
package so

// PathKind 值存在哪里 / 用哪个方向的关系。
type PathKind uint8

const (
	PathSystem PathKind = iota // 节点保留列: id / type / display / …
	PathField                  // 类型的标量字段（存 fields JSON）
	PathOutRef                 // 出边引用: ->field
	PathInRef                  // 入边引用: <-type.field（SourceType 必填）
)

// Path 一条 schema 路径。SourceType 只在入边引用时需要。
type Path struct {
	Kind       PathKind `json:"kind"`
	SourceType string   `json:"source_type,omitempty"`
	Field      string   `json:"field"`
}

// Expr 查询 AST。只有两个实现 —— 见包注释。
type Expr interface {
	queryExpr()
}

type LogicOp string

const (
	OpAnd LogicOp = "and"
	OpOr  LogicOp = "or"
	OpNot LogicOp = "not"
)

// Logic 组合子表达式。**只有这三个算符是结构性的** —— 它们的参数是子表达式,
// 其余算符的参数是值（由算符自己按位置解释）。
type Logic struct {
	Op   LogicOp
	Args []Expr
}

func (Logic) queryExpr() {}

// Predicate 一次算符调用。Name 交给 core 的注册表解释; Args 原样保留
// （值列表与子表达式由算符自己按位置判断 —— 树里没有"这是调用/这是值"的标记）。
type Predicate struct {
	Name string
	Args []any
}

func (Predicate) queryExpr() {}

// SortField 一个已校验的排序请求。稳定序（id）由 core 追加。
type SortField struct {
	Path Path `json:"path"`
	Desc bool `json:"desc"`
}

// Page 页码分页。使用时 Number 与 Size 必须为正。
type Page struct {
	Number int `json:"number"`
	Size   int `json:"size"`
}
