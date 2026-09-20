package types

import (
	"fmt"
)

// SystemField 是节点系统列（id / type / revision / fields / created_at /
// updated_at）的声明：可查能力 + 查询值校验。
//
// 存在的理由和 Kind 的 QueryOps 一样：**能力要声明，不许在编译期按名字猜**。
// 以前这三件事散在三份手写清单里（nodeColumns / reservedField / query_compile 里的
// if 链），加一列就要改三处，还漏掉了"系统列能不能排序"这条校验。
//
// Ops 全 false = 该列不可查（fields 是 JSON 容器，只用于穿透路径）。
type SystemField struct {
	Name string
	Ops  QueryOps
	// Validate 查询值校验。注意查询值域 ≠ 存储值域：id 接受 int/float/数字串，
	// 时间列只接受 **Unix 秒（整数）**—— 时间列本身是 int64, 拿字符串/time.Time 去比
	// 会变成"字符串 vs 整数"（SQLite 里 TEXT 永远大于 INTEGER）, 静默给出错的结果。
	Validate func(v any) error
}

// systemFields 是唯一声明处 —— IsNodeColumn / IsReservedField / 查询编译 / 排序校验
// 都从这里派生。
var systemFields = []SystemField{
	{Name: "id", Ops: QueryOps{Equal: true, Ordered: true, Sortable: true}, Validate: validateIDValue},
	{Name: "type", Ops: QueryOps{Equal: true, Text: true, Sortable: true}, Validate: validateStringValue},
	// address 是**生成列**（由注入的 address 字段投影）—— 地址查询与排序走它才用得上
	// 唯一索引; 同名的字段路径 `$address` 语义相同但走 json_extract（全表扫）。
	{Name: AddressField, Ops: QueryOps{Equal: true, Text: true, Sortable: true}, Validate: validateStringValue},
	{Name: "revision", Ops: QueryOps{Equal: true, Ordered: true, Sortable: true}, Validate: validateIDValue},
	{Name: "fields", Ops: QueryOps{}},
	{Name: "created_at", Ops: QueryOps{Equal: true, Ordered: true, Sortable: true}, Validate: validateTimeValue},
	{Name: "updated_at", Ops: QueryOps{Equal: true, Ordered: true, Sortable: true}, Validate: validateTimeValue},
}

var systemFieldIndex = func() map[string]SystemField {
	index := make(map[string]SystemField, len(systemFields))
	for _, f := range systemFields {
		index[f.Name] = f
	}
	return index
}()

// SystemField 取系统列声明（与 Kind(name) 对称）；不存在返回 ok=false。
func (t *Types) SystemField(name string) (SystemField, bool) {
	f, ok := systemFieldIndex[name]
	return f, ok
}

// IsNodeColumn 该名字是否是节点列（穿透路径第二段：无 $. 前缀即列）。
func IsNodeColumn(name string) bool {
	_, ok := systemFieldIndex[name]
	return ok
}

// IsReservedField 类型定义里不能使用的字段名（会被节点列遮蔽）。
func IsReservedField(name string) bool { return IsNodeColumn(name) }

func validateStringValue(v any) error {
	if _, ok := v.(string); !ok {
		return fmt.Errorf("expects string")
	}
	return nil
}

func validateIDValue(v any) error {
	if _, err := ToID(v); err != nil {
		return fmt.Errorf("expects integer")
	}
	return nil
}

// validateTimeValue 时间列的查询值: 只收 Unix 秒（整数）。
//
// 为什么不能收 time.Time/字符串: 时间列在库里是 int64, 绑一个字符串进去比较会变成
// "TEXT vs INTEGER" —— SQLite 的规则是 TEXT > INTEGER 恒真, 于是 `updated_at > '2026-…'`
// 会把每一行都算成命中（或一行都不中）, **不报错**。要传 time.Time 就先 `.Unix()`。
func validateTimeValue(v any) error {
	if _, ok := UnixSeconds(v); ok {
		return nil
	}
	return fmt.Errorf("expects unix seconds (integer), got %T(%v) —— time.Time 请先 .Unix()", v, v)
}
