// 类型内唯一键（类型声明 capabilities.unique 时才有）。
//
// 键 = JSON 数组, 第一个元素是类型名:
//
//	["member","91440300MA5xxxx"]        标量唯一字段
//	["position","12","34"]              两个单引用（人 + 公司）
//
// 为什么不是分隔符拼串: 值里带分隔符就会撞（member/a:b 与 member:a/b 同键）;
// JSON 的引号与转义天然无歧义, 而且冲突报错时能原样打印给人看。
//
// 为什么不是生成列: 键里的**字段名每个类型都不同**（member 是 credit_code,
// 任职是 person+company）, 而生成列的表达式写死在 DDL 里 ⇒ 只能由写路径算。
//
// 引用按**目标节点 id** 进键（不按显示名 —— 名字会改, id 不会）。
// 任一部分缺失/为空 ⇒ 整键为 NULL ⇒ **不参与唯一**（"没填全 = 不约束"）。
package core

import (
	"encoding/json"
	"fmt"

	"github.com/kran/gcmv3/types"
)

// uniqView 唯一字段的取值视图: 标量是值, 引用是目标 id（int64 或 []int64）。
//
// create 从 splitFields 的 (scalar, refs) 拼; patch 从"读投影（引用 id 已在
// fields 里）+ patch"拼 —— 两边都给同一个形状, projectUniq 不必知道来源。
func uniqView(scalar map[string]any, refs map[string][]int64) map[string]any {
	view := make(map[string]any, len(scalar)+len(refs))
	for name, value := range scalar {
		view[name] = value
	}
	for name, ids := range refs {
		switch len(ids) {
		case 0:
			// 没落边 = 这个引用是空的（不设 key ⇒ 投影时当"缺失"）
		case 1:
			view[name] = ids[0]
		default:
			view[name] = ids // 多值: 由类型校验挡在门外（refs 不许当唯一键）
		}
	}
	return view
}

// projectUniq 算唯一键（类型没声明 unique ⇒ 返回 nil）。
func (s *GCM) projectUniq(td types.TypeDef, view map[string]any) *string {
	names := td.Capabilities.Unique
	if len(names) == 0 {
		return nil
	}
	parts := make([]string, 0, len(names)+1)
	parts = append(parts, td.Name)
	for _, name := range names {
		part, ok := uniqPart(view[name])
		if !ok {
			return nil // 缺一块 ⇒ 整键 NULL（不参与唯一）
		}
		parts = append(parts, part)
	}
	encoded, err := json.Marshal(parts)
	if err != nil {
		// 只有字符串切片, Marshal 不会失败; 真失败了也不该静默写个半截键
		return nil
	}
	text := string(encoded)
	return &text
}

// uniqPart 一个部分 → 字符串。缺失/空值/引用没落边 ⇒ ok=false。
func uniqPart(value any) (string, bool) {
	switch typed := value.(type) {
	case nil:
		return "", false
	case string:
		if typed == "" {
			return "", false
		}
		return typed, true
	case int64:
		return fmt.Sprintf("%d", typed), true
	case int:
		return fmt.Sprintf("%d", typed), true
	case float64:
		if typed == 0 {
			return "", false
		}
		return fmt.Sprintf("%d", int64(typed)), true
	case bool:
		if !typed {
			return "", false
		}
		return "true", true
	case []int64:
		if len(typed) != 1 {
			return "", false // 多值引用当唯一键 —— 类型校验已经拒了, 这里兜底不约束
		}
		return fmt.Sprintf("%d", typed[0]), true
	}
	return "", false
}
