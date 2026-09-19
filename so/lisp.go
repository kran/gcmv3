package so

import (
	"fmt"
	"strconv"
	"strings"
)

// Lisp 文本前端: 把 s-expr 文本读成**前端树**（tree.go 定义的那两种形态）。
//
//	(and (= $state "published") (in ->categories [1 2 3]))
//
// 与 JSON 数组形态是同一棵树 —— 只差一对括号:
//
//	["and", ["=", "$state", "published"], ["in", "->categories", [1,2,3]]]
//
// 所以两种前端共用 Reader: 换前端只换 tokenizer, 读树逻辑不动。
//
// 取值写法:  status(列) / $name(字段) / ->ref(出边) / <-type.field(入边)
// 值:        数字 / "字符串" / true / false / null / [值 …] / {:占位符}
func ParseLisp(src string, params map[string]any) (Expr, error) {
	tree, err := parseLispTree(src, params)
	if err != nil {
		return nil, err
	}
	return NewReader().Read(tree)
}

// ParseWhere lisp 源码 → Where（**前端树**形态）—— 收 HTTP 的 `filter=…` 用它。
//
// 与 ParseLisp 的差别只是"读到哪一步": 这里产出的是**条件**本身, 与 so.P/AND/OR
// 的产物是同一种东西, 照样进 NodeQuery.Where —— 于是算符名/字段名/复杂度的校验
// 仍然只有编译期那一处（不在这里再校验一遍）。
func ParseWhere(src string, params map[string]any) (Where, error) {
	tree, err := parseLispTree(src, params)
	if err != nil {
		return Where{}, err
	}
	return Where{tree: tree}, nil
}

// parseLispTree lisp 源码 → 前端树（两个出口共用同一套 tokenizer 与占位符解析）。
func parseLispTree(src string, params map[string]any) (any, error) {
	if len(src) > MaxFilterBytes {
		return nil, fmt.Errorf("query: lisp expression exceeds %d bytes", MaxFilterBytes)
	}
	parser := &lispParser{src: src}
	parser.skipSpace()
	tree, err := parser.parseNode(0)
	if err != nil {
		return nil, err
	}
	parser.skipSpace()
	if parser.pos != len(parser.src) {
		return nil, fmt.Errorf("query: lisp unexpected trailing input at %d", parser.pos)
	}
	return resolvePlaceholders(tree, params)
}

type lispParser struct {
	src string
	pos int
}

// parseNode 读一个节点: (调用) / [值数组] / 原子。返回前端树的一个节点。
func (p *lispParser) parseNode(depth int) (any, error) {
	if depth > MaxFilterDepth {
		return nil, fmt.Errorf("query: lisp nesting exceeds %d", MaxFilterDepth)
	}
	p.skipSpace()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("query: lisp unexpected end")
	}
	switch p.src[p.pos] {
	case '(':
		p.pos++
		p.skipSpace()
		head, err := p.parseToken()
		if err != nil {
			return nil, err
		}
		out := []any{head}
		for {
			p.skipSpace()
			if p.pos >= len(p.src) {
				return nil, fmt.Errorf("query: lisp unterminated (")
			}
			if p.src[p.pos] == ')' {
				p.pos++
				return out, nil
			}
			arg, err := p.parseNode(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, arg)
		}
	case ')':
		return nil, fmt.Errorf("query: lisp unexpected ) at %d", p.pos)
	case ']':
		return nil, fmt.Errorf("query: lisp unexpected ] at %d", p.pos)
	case '[':
		p.pos++
		items := make([]any, 0, 4)
		for {
			p.skipSpace()
			if p.pos >= len(p.src) {
				return nil, fmt.Errorf("query: lisp unterminated [")
			}
			if p.src[p.pos] == ']' {
				p.pos++
				return items, nil
			}
			if len(items) >= MaxSetValues {
				return nil, fmt.Errorf("query: lisp array exceeds %d items", MaxSetValues)
			}
			item, err := p.parseNode(depth + 1)
			if err != nil {
				return nil, err
			}
			// 数组里只放值: 嵌套调用在这里是写法错误（要的是集合, 不是条件）
			if _, isCall := item.([]any); isCall {
				return nil, fmt.Errorf("query: lisp array elements must be values")
			}
			items = append(items, item)
		}
	default:
		token, err := p.parseToken()
		if err != nil {
			return nil, err
		}
		return parseAtom(token), nil
	}
}

func (p *lispParser) parseToken() (string, error) {
	p.skipSpace()
	start := p.pos
	if p.pos < len(p.src) && p.src[p.pos] == '"' {
		p.pos++
		escaped := false
		for p.pos < len(p.src) {
			char := p.src[p.pos]
			if escaped {
				escaped = false
				p.pos++
				continue
			}
			if char == '\\' {
				escaped = true
				p.pos++
				continue
			}
			if char == '"' {
				p.pos++
				return p.src[start:p.pos], nil
			}
			p.pos++
		}
		return "", fmt.Errorf("query: lisp unterminated string at %d", start)
	}
	for p.pos < len(p.src) {
		char := p.src[p.pos]
		if strings.ContainsRune(" \t\n()[]", rune(char)) {
			break
		}
		p.pos++
	}
	if start == p.pos {
		return "", fmt.Errorf("query: lisp empty token at %d", p.pos)
	}
	return p.src[start:p.pos], nil
}

func (p *lispParser) skipSpace() {
	for p.pos < len(p.src) && strings.ContainsRune(" \t\n", rune(p.src[p.pos])) {
		p.pos++
	}
}

// parseAtom 一个 token → 值（引号字符串 / 占位符 / 数字 / 布尔 / null / 裸 token）。
// 裸 token 保持字符串 —— 是不是路径由算符按位置决定（Reader.Path）。
func parseAtom(token string) any {
	if len(token) >= 2 && token[0] == '"' && token[len(token)-1] == '"' {
		value, err := strconv.Unquote(token)
		if err == nil {
			return value
		}
	}
	if strings.HasPrefix(token, "{:") && strings.HasSuffix(token, "}") {
		return placeholder{name: token[2 : len(token)-1]}
	}
	if integer, err := strconv.ParseInt(token, 10, 64); err == nil {
		return integer
	}
	if number, err := strconv.ParseFloat(token, 64); err == nil {
		return number
	}
	switch token {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	return token
}

// placeholder 文本前端的 {:name}。**只有文本前端有它** —— JSON 前端直接在值的位置
// 写值, 所以占位符的解析留在 tokenizer 这一侧, 读树逻辑不认识它。
type placeholder struct{ name string }

// resolvePlaceholders 把占位符换成调用方给的值（含数组内层）。
// 参数里没有的名字直接报错 —— 模板少传一个参数的错误必须在解析时就炸。
func resolvePlaceholders(value any, params map[string]any) (any, error) {
	switch item := value.(type) {
	case placeholder:
		resolved, ok := params[item.name]
		if !ok {
			return nil, fmt.Errorf("query: lisp placeholder {:%s} not bound", item.name)
		}
		return resolved, nil
	case []any:
		out := make([]any, len(item))
		for i := range item {
			next, err := resolvePlaceholders(item[i], params)
			if err != nil {
				return nil, err
			}
			out[i] = next
		}
		return out, nil
	default:
		return value, nil
	}
}
