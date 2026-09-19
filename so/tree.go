package so

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// 复杂度上限。前端的形状与文本长短都能被外部控制（URL 参数 / 模板 / 客户端 JSON）,
// 所以"能写多少"必须在**读树**这一层封顶。
const (
	MaxFilterBytes = 4096 // 文本前端: 表达式字节数
	MaxFilterNodes = 256  // 所有前端: 节点数
	MaxFilterDepth = 12   // 所有前端: 嵌套深度
	MaxSetValues   = 100  // 集合元素个数（由算符在编译期检查）
)

// Reader 结构读取器: 前端树 → AST。
//
// 它只认**结构**:
//
//	[]any           一次调用（首项是算符名）
//	其它            一个值
//
// and / or / not 的子项是**子表达式**, 递归读; 其余算符的参数原样保留。
// 这个区分必须由结构决定而不能靠"参数看起来像不像调用": 在算符眼里
// ["in", "->categories", [1,2,3]] 的 [1,2,3] 是**值列表**, 不是一次调用。
//
// Reader **有状态**（节点计数）—— 一次解析一个实例。
type Reader struct {
	nodes int
}

func NewReader() *Reader { return &Reader{} }

// Read 读一棵前端树。
func (r *Reader) Read(tree any) (Expr, error) {
	value := normalizeTree(tree)
	if value == nil {
		return nil, fmt.Errorf("query: expression required")
	}
	return r.expr(value, 0)
}

func (r *Reader) expr(value any, depth int) (Expr, error) {
	if depth > MaxFilterDepth {
		return nil, fmt.Errorf("query: expression exceeds depth %d", MaxFilterDepth)
	}
	r.nodes++
	if r.nodes > MaxFilterNodes {
		return nil, fmt.Errorf("query: expression exceeds %d nodes", MaxFilterNodes)
	}
	call, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("query: condition must be [operator args…], got %T", value)
	}
	if len(call) == 0 {
		return nil, fmt.Errorf("query: empty condition")
	}
	name, ok := call[0].(string)
	if !ok || name == "" {
		return nil, fmt.Errorf("query: operator name must be a non-empty string, got %T", call[0])
	}
	args := call[1:]
	switch name {
	case "and", "or":
		if len(args) == 0 {
			return nil, fmt.Errorf("query: %s requires arguments", name)
		}
		children := make([]Expr, len(args))
		for i := range args {
			child, err := r.expr(args[i], depth+1)
			if err != nil {
				return nil, err
			}
			children[i] = child
		}
		op := OpAnd
		if name == "or" {
			op = OpOr
		}
		return Logic{Op: op, Args: children}, nil
	case "not":
		if len(args) != 1 {
			return nil, fmt.Errorf("query: not takes 1 argument(s), got %d", len(args))
		}
		child, err := r.expr(args[0], depth+1)
		if err != nil {
			return nil, err
		}
		return Logic{Op: OpNot, Args: []Expr{child}}, nil
	default:
		// 无参算符 ⇒ nil（而不是空切片）: 三种写法产出的 AST 要能逐位比较。
		if len(args) == 0 {
			args = nil
		}
		return Predicate{Name: name, Args: args}, nil
	}
}

// ParsePath 一个路径 token → Path:
//
//	$name           类型的标量字段（存 fields JSON）
//	name            节点保留列（id / type / display / …）
//	->name          出边引用
//	<-type.field    入边引用（来源类型必须显式 —— 同名字段在不同类型上含义不同）
func ParsePath(value any) (Path, error) {
	token, ok := value.(string)
	if !ok || token == "" {
		return Path{}, fmt.Errorf("query: path must be a non-empty string, got %T", value)
	}
	switch {
	case strings.HasPrefix(token, "->"):
		name := strings.TrimPrefix(token, "->")
		if name == "" {
			return Path{}, fmt.Errorf("query: ref path must be ->field, got %q", token)
		}
		return Path{Kind: PathOutRef, Field: name}, nil
	case strings.HasPrefix(token, "<-"):
		sourceType, field, found := strings.Cut(strings.TrimPrefix(token, "<-"), ".")
		if !found || sourceType == "" || field == "" {
			return Path{}, fmt.Errorf("query: incoming path must be <-type.field, got %q", token)
		}
		return Path{Kind: PathInRef, SourceType: sourceType, Field: field}, nil
	case strings.HasPrefix(token, "$"):
		name := strings.TrimPrefix(token, "$")
		if name == "" || strings.HasPrefix(name, ".") {
			return Path{}, fmt.Errorf("query: dynamic path must be $field, got %q", token)
		}
		return Path{Kind: PathField, Field: name}, nil
	default:
		return Path{Kind: PathSystem, Field: token}, nil
	}
}

// normalizeTree 把前端树归一成**唯一形态**: 数组 → []any, 整数 → int64,
// 浮点 → float64, 其余原样。
//
// 一条实现, 两处用:
//  1. 三种写法（Go 构造器 / Lisp / JSON）产出的树必须**逐位相同**。JSON 解出
//     json.Number + []any, Lisp 解出 int64, Go 手里是 int / []int64 —— 不归一
//     就会"同一个查询三种行为"（切片甚至会被当成单个值）。
//  2. 编译器因此可以依赖这个不变式: 树里只有 int64 / float64。
func normalizeTree(value any) any {
	switch item := value.(type) {
	case nil:
		return nil
	case json.Number:
		if integer, err := item.Int64(); err == nil {
			return integer
		}
		if number, err := item.Float64(); err == nil {
			return number
		}
		return item.String()
	case []any:
		out := make([]any, len(item))
		for i := range item {
			out[i] = normalizeTree(item[i])
		}
		return out
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]any, reflected.Len())
		for i := range out {
			out[i] = normalizeTree(reflected.Index(i).Interface())
		}
		return out
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return reflected.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(reflected.Uint())
	case reflect.Float32:
		return reflected.Float()
	default:
		return value
	}
}
