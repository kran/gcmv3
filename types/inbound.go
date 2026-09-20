package types

import (
	"fmt"
	"strings"
)

// InboundSpec 一条反向引用声明（写在一个类型的 admin.inbounds 里）。
type InboundSpec struct {
	// Ref 哪个 "类型.字段" 指向本类型（如 article.category）。
	Ref string `yaml:"ref" json:"ref"`
	// Fields 在这个小列表里显示哪些列; 不写 = 用对方的 admin.columns。
	Fields []string `yaml:"fields,omitempty" json:"fields,omitempty"`
}

// Inbound 解析好的反向引用（运行期用: 端点按它查、前端按它渲染）。
type Inbound struct {
	Spec   string   `json:"spec"`             // "article.category"（前端分组 key）
	Type   string   `json:"type"`             // 引用方类型
	Field  string   `json:"field"`            // 引用方字段
	Label  string   `json:"label,omitempty"`  // 引用方类型的后台名
	Fields []string `json:"fields,omitempty"` // 要显示的列（已补缺省）
}

// parseInboundRef 拆 "类型.字段"（校验期与运行期共用同一口径）。
func parseInboundRef(spec string) (string, string, error) {
	trimmed := strings.TrimSpace(spec)
	otherType, otherField, ok := strings.Cut(trimmed, ".")
	if !ok || otherType == "" || otherField == "" {
		return "", "", fmt.Errorf("admin.inbounds 里的 %q 该写成 \"类型.字段\"（如 article.category）", spec)
	}
	return otherType, otherField, nil
}

// Inbounds 该类型声明的反向引用（fields 已按"缺省 = 对方的 admin.columns"补齐）。
func (t *Types) Inbounds(typeName string) []Inbound {
	td, ok := t.defs[typeName]
	if !ok {
		return nil
	}
	out := make([]Inbound, 0, len(td.Admin.Inbounds))
	for _, spec := range td.Admin.Inbounds {
		otherType, otherField, err := parseInboundRef(spec.Ref)
		if err != nil {
			continue // 加载期校验过（这里只在配置坏掉时兜底）
		}
		in := Inbound{Spec: spec.Ref, Type: otherType, Field: otherField, Fields: spec.Fields}
		if other, ok := t.defs[otherType]; ok {
			in.Label = other.Admin.Label
			if in.Label == "" {
				in.Label = otherType
			}
			if len(in.Fields) == 0 {
				in.Fields = other.Admin.Columns // 缺省: 和内容列表看一样的列
			}
		}
		out = append(out, in)
	}
	return out
}

// validateInbounds 加载**完成之后**统一校验（此刻 defs 才齐 —— 逐类型阶段查不到
// "另一个类型", 会把正常声明误报成"类型不存在"）。
func (t *Types) validateInbounds() error {
	for typeName, td := range t.defs {
		for _, spec := range td.Admin.Inbounds {
			otherType, otherField, err := parseInboundRef(spec.Ref)
			if err != nil {
				return fmt.Errorf("types: type %q: %w", typeName, err)
			}
			other, ok := t.defs[otherType]
			if !ok {
				return fmt.Errorf("types: type %q: admin.inbounds 里的类型 %q 不存在", typeName, otherType)
			}
			f, ok := FieldByName(other, otherField)
			if !ok {
				return fmt.Errorf("types: type %q: 类型 %q 没有字段 %q（admin.inbounds）",
					typeName, otherType, otherField)
			}
			// 指向本类型才算"入边" —— 拼错的话列表会永远空着（最难查的那种）
			if f.To != typeName {
				return fmt.Errorf("types: type %q: admin.inbounds 里的 %q 不是指向本类型的引用（它 to=%q）",
					typeName, spec.Ref, f.To)
			}
			// 要显示的列必须真存在（拼错 ⇒ 界面上少一列, 静默的那种）
			for _, name := range spec.Fields {
				if _, ok := FieldByName(other, name); !ok {
					return fmt.Errorf("types: type %q: admin.inbounds 里 %q 的 fields 提到 %q, 但类型 %q 没有这个字段",
						typeName, spec.Ref, name, otherType)
				}
			}
		}
	}
	return nil
}
