package so

// Where 一个前端树节点 = 一个 s-expr。
//
// **一切皆值**: 每个构造器都返回一个建好的节点, 所以不存在"先挂载后填充"的窗口
// （那正是静默丢数据的来源: 父层手里的切片头长度永远是挂载那一刻的）。
//
//	so.AND(
//		so.P("=", "$state", "published"),
//		so.OR(
//			so.P("in", "->categories", ids),
//			so.P("exists", "<-article.categories"),
//		),
//		so.IF(actor.Role != "admin", "=", "$author", actor.NodeID),
//	)
type Where struct {
	tree any
}

// P 一条叶子调用。
//
//	query.P("=", "$state", "published")
//	query.P("match", "人工智能")          // 插件注册的谓词一样走它
//
// 算符名与参数拼错不在这里拦 —— 读树（Expr）时才报, 措辞指向用户的输入
// （"unknown operator adn"）。这就是"校验一处"。
func P(name string, args ...any) Where {
	tree := make([]any, 0, len(args)+1)
	tree = append(tree, name)
	for _, arg := range args {
		tree = append(tree, normalizeTree(arg))
	}
	return Where{tree: tree}
}

// IF 条件包含: 条件为假 ⇒ **空项**, 上层组合器当场丢掉 —— 树里不会出现 null。
//
// 所以语言（lisp / JSON）**不需要 null 的语义**: 条件包含是构造期的事,
// "不写这一项"与"写了个空项"产出完全相同的树。
//
//	so.AND(
//		so.P("=", "$state", "published"),
//		so.IF(actor.Role != "admin", so.P("=", "$author", actor.NodeID)),
//		so.IF(actor.Can("editor"), so.OR(
//			so.P("=", "$state", "draft"),
//			so.P("=", "$author", actor.NodeID),
//		)),
//	)
//
// 第二个参数是**任意节点**（叶子 P / 组合 OR / NOT / REF）—— 用 so.AND(…) 包
// 一组条件。不接变参是为了不发明"多子项自动 AND"这种隐式规则。
func IF(cond bool, item Where) Where {
	if !cond {
		return Where{}
	}
	return item
}

// absent 这一项不存在（IF 条件为假 / 零值 Where）。
// 它**永远不进树** —— 组合器当场丢掉, 所以前端树里没有任何"空"的表示。
func (w Where) absent() bool { return w.tree == nil }

// IsZero 零值（没有条件）。调用方用它区分"不过滤"与"过滤条件为空"。
func (w Where) IsZero() bool { return w.tree == nil }

// AND / OR 组合一组条件。空项被丢掉; **全空 ⇒ 空组合**, 读树时报错
// （空的 and 编译成恒真 = 放行方向, 必须响 —— 所以要报错而不是变成空项）。
func AND(items ...Where) Where { return node("and", items) }
func OR(items ...Where) Where  { return node("or", items) }

// NOT 取反。**单个**条件 —— 个数由签名保证。
// 空项进来 ⇒ 空项出去（包一层 NOT 不会把'没有条件'变成'一个条件'）。
func NOT(item Where) Where {
	if item.absent() {
		return Where{}
	}
	return Where{tree: []any{"not", item.tree}}
}

// REF 打开一个关系, 子条件作用在目标节点上（`->categories` 里存在一个 …）。
// 不叫 Related 是因为 AST 节点类型已经叫 Related。空项同样传出去。
func REF(path string, item Where) Where {
	if item.absent() {
		return Where{}
	}
	return Where{tree: []any{"ref", path, item.tree}}
}

func node(operator string, items []Where) Where {
	tree := make([]any, 0, len(items)+1)
	tree = append(tree, operator)
	for _, item := range items {
		if item.absent() {
			continue
		}
		tree = append(tree, item.tree)
	}
	return Where{tree: tree}
}

// Tree 前端树 —— 喂 Reader.Read, 或 json.Marshal 成 JSON 形态。
func (w Where) Tree() any { return w.tree }

// Expr 读成 AST（结构 + 复杂度上限）。算符认不认、参数几个要在编译期才知道 ——
// 算符词表在 core, 不在这里。
func (w Where) Expr() (Expr, error) {
	return NewReader().Read(w.tree)
}
