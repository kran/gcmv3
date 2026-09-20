package types

import (
	"fmt"
	"strings"
)

// Inbound 一条反向引用声明: "谁引用我"。
//
// 声明写法是 "类型.字段"（如 article.category）—— 那个字段是指向本类型的 ref。
// 后台编辑器据此列出引用本节点的那些节点（只读: 边的所有权在对面）。
type Inbound struct {
	Spec  string `json:"spec"`            // "article.category" 原样（前端分组 key）
	Type  string `json:"type"`            // 引用方类型
	Field string `json:"field"`           // 引用方字段
	Label string `json:"label,omitempty"` // 引用方类型的后台名（Types.Inbounds 填）
}

// ParseInbound 解析 "类型.字段"（校验期与运行期共用, 口径一致）。
func ParseInbound(spec, selfType string) (Inbound, error) {
	otherType, otherField, ok := strings.Cut(strings.TrimSpace(spec), ".")
	if !ok || otherType == "" || otherField == "" {
		return Inbound{}, &InboundSpecError{Spec: spec, Self: selfType}
	}
	return Inbound{Spec: spec, Type: otherType, Field: otherField}, nil
}

// InboundSpecError 声明写法不合法（要 "类型.字段"）。
type InboundSpecError struct {
	Spec string
	Self string
}

func (e *InboundSpecError) Error() string {
	return "admin.inbounds 里的 " + e.Spec + " 该写成 \"类型.字段\"（如 article.category）"
}

// Inbounds 该类型声明的反向引用（带引用方类型的中文名, 前端直接显示）。
func (t *Types) Inbounds(typeName string) []Inbound {
	td, ok := t.defs[typeName]
	if !ok {
		return nil
	}
	out := make([]Inbound, 0, len(td.Admin.Inbounds))
	for _, spec := range td.Admin.Inbounds {
		parsed, err := ParseInbound(spec, typeName)
		if err != nil {
			continue // 加载期已经校验过（这里只在配置坏掉时兜底）
		}
		if other, ok := t.defs[parsed.Type]; ok {
			parsed.Label = other.Admin.Label
			if parsed.Label == "" {
				parsed.Label = parsed.Type
			}
		}
		out = append(out, parsed)
	}
	return out
}

// validateInbounds 加载**完成之后**统一校验（此刻 t.defs 才齐 —— 逐类型阶段查不到
// "另一个类型", 会把一个正常声明误报成"类型不存在"）。
func (t *Types) validateInbounds() error {
	for typeName, td := range t.defs {
		for _, spec := range td.Admin.Inbounds {
			parsed, err := ParseInbound(spec, typeName)
			if err != nil {
				return fmt.Errorf("types: type %q: %w", typeName, err)
			}
			other, ok := t.defs[parsed.Type]
			if !ok {
				return fmt.Errorf("types: type %q: admin.inbounds 里的类型 %q 不存在", typeName, parsed.Type)
			}
			f, ok := FieldByName(other, parsed.Field)
			if !ok {
				return fmt.Errorf("types: type %q: 类型 %q 没有字段 %q（admin.inbounds）",
					typeName, parsed.Type, parsed.Field)
			}
			// 指向本类型才算"入边" —— 拼错的话列表会永远空着（最难查的那种）
			if f.To != typeName {
				return fmt.Errorf("types: type %q: admin.inbounds 里的 %q 不是指向本类型的引用（它 to=%q）",
					typeName, spec, f.To)
			}
		}
	}
	return nil
}
