package core

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"

	"github.com/spf13/cast"
)

// ── Node 通用列 ───────────────────────────────
//
// 类型字段名不得与这些保留名冲突（types 校验期拒绝）:
//   id / type / revision / fields / created_at / updated_at

// Node 节点 — 值模型（读/模板/JSON 展示用）。
//
// 节点没有"标签列"这种东西: 想让界面显示什么, 就声明一个普通字段（如 name /
// title）—— 由展示层决定拿哪个字段当标签。
//
// 写路径差量使用 NodePatch：
//
//	CreateNode(n *Node)  全量插入
//	PatchNode(id, patch) 非 nil 列写 + fields json_patch merge
//
// Fields 类型字段（动态 — 类型定义声明; ref 引用在 edges, 不在此）。
// Scan/Value: DB JSON 字符串 ↔ map 自动转换（dba 扫/插直接可用）。
//
// ── cast 快捷方法（容错取值 — 字段值类型多变: JSON float64/int64/string） ──
type Fields map[string]any

// Str 取字符串（nil→""; 数字→字符串）。
func (f Fields) Str(name string) string { return cast.ToString(f[name]) }

// Int 取整数（字符串/float64→int64; 非法→0）。
func (f Fields) Int(name string) int64 { return cast.ToInt64(f[name]) }

// Float 取浮点。
func (f Fields) Float(name string) float64 { return cast.ToFloat64(f[name]) }

// Bool 取布尔。
func (f Fields) Bool(name string) bool { return cast.ToBool(f[name]) }

// Has 字段是否存在（含 null 值）。
func (f Fields) Has(name string) bool { _, ok := f[name]; return ok }

// Map 取嵌套 map。
func (f Fields) Map(name string) map[string]any { return cast.ToStringMap(f[name]) }

// Slice 取数组（nil→空切片）。
// Slice 取多值字段。两种形态都认：读投影给的是 []int64，JSON 解码给的是 []any。
func (f Fields) Slice(name string) []any {
	switch v := f[name].(type) {
	case nil:
		return nil
	case []int64:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	default:
		return cast.ToSlice(v)
	}
}

// Scan 从 DB JSON 还原。
func (f *Fields) Scan(v any) error {
	if v == nil {
		*f = Fields{}
		return nil
	}
	var b []byte
	switch t := v.(type) {
	case []byte:
		b = t
	case string:
		b = []byte(t)
	default:
		return fmt.Errorf("core: fields scan: unexpected type %T", v)
	}
	m := Fields{}
	if len(b) > 0 && string(b) != "null" {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("core: fields scan: %w", err)
		}
	}
	*f = m
	return nil
}

// Value 存库为 JSON。
func (f Fields) Value() (driver.Value, error) {
	if f == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(f)
}

// Node 节点 — 值模型（读/模板/JSON/DB 直接可用）。
type Node struct {
	ID        int64  `db:"id,omitempty" json:"id"` // omitempty: 插入跳零值走自增
	Type      string `db:"type" json:"type"`
	Revision  int64  `db:"revision" json:"revision"`
	CreatedAt int64  `db:"created_at" json:"created_at"`
	UpdatedAt int64  `db:"updated_at" json:"updated_at"`

	// 类型字段（Scan/Value 自动 JSON 转换；ref/refs 存 edges）
	Fields Fields `db:"fields" json:"fields"`

	// Address 是**生成列**（由注入的 address 字段投影）—— 内部用: 地址的唯一性与
	// 定位（GetNode）走它才用得上索引。**不对外**: 地址值本身是字段（在 Fields 里）,
	// 授权/掩码按字段走 —— 派生值不能绕开字段规则泄漏。
	//
	// omitempty: 生成列**不能写** —— 插入时跳过（同 ID 的套路）；读时正常扫出来。
	// 指针: 没有地址就是 NULL（SQLite 里 NULL 互不相等 ⇒ 天然不参与唯一）。
	Address *string `db:"address,omitempty" json:"-"`

	// Expand 引用展开容器（typed Expand 填充 — 不落库）: map[路径 key] → *Node / []*Node
	Expand map[string]any `db:"-" json:"expand,omitempty"`
	// Extra 渲染期附加数据（HookNodeEnrich 填充 — 不落库）: url 注入、高亮等
	Extra map[string]any `db:"-" json:"extra,omitempty"`

	// ── 读期事实（web 读层一次算好，随节点下发；引擎与写入路径不关心）──
	//
	//	Masked    类型声明里有、但本次响应被读规则裁掉的字段（"看起来空"不等于"没值"）
	//	Editable  本 actor 在此节点上**实际可写**的字段（写侧 Grant ∩ 可读；见 web 授权）
	//
	// **不下发 omitempty**: 两个事实都必须是**数组**（空就是 `[]`）——
	// "字段不在响应里"曾经被解释成"没有限制"，于是 editable 一缺席, 前端就把
	// 只读表单画成可编辑的（fail-open: 界面给控件、一存 403）。缺 facts 不是
	// "随便写"，所以它不能没有答案。
	Masked   []string `db:"-" json:"masked"`
	Editable []string `db:"-" json:"editable"`
}

// Field 类型字段值（无 → nil）。
func (n *Node) Field(name string) any {
	if n.Fields == nil {
		return nil
	}
	return n.Fields[name]
}

type NodePatch struct {
	// Type 目标节点的类型 —— **由引擎填**（`json:"-"`：客户端传不进来），
	// 供 before_update 钩子判断"这个类型的规则/投影"。业务代码别自己设。
	Type     string         `json:"-"`
	Revision *int64         `json:"revision"`
	Fields   map[string]any `json:"fields,omitempty"`
}
