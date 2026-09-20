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
	"strconv"

	"github.com/kran/gcmv3/types"
	"github.com/spf13/cast"
)

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

// uniqPart 一个部分 → 字符串。
//
// 一切转字符串（要的就是这个）: 数字/布尔/文本一视同仁; 引用是目标 id（int64 或
// []int64 —— 单引用就是单元素, 在这里拆开）。
//
// 空字符串 = "没填" ⇒ ok=false（"没填全 = 不约束"）。这条规则只有一处, 所以
// create 与 patch 不可能对"什么算空"有不同理解。
func uniqPart(value any) (string, bool) {
	if ids, ok := value.([]int64); ok {
		if len(ids) != 1 {
			return "", false // 多值当唯一键: 类型校验已拒, 这里兜底不约束
		}
		return strconv.FormatInt(ids[0], 10), true
	}
	text := cast.ToString(value)
	return text, text != ""
}
