package types

import (
	"fmt"
	"slices"
	"strings"
)

// multiselectKind 多选枚举（选项来自字段声明的 options; 值 = 选项字符串的数组）。
//
// 与 select 的唯一差别是"值是数组"。**查询能力故意留空**：数组元素的反查内核不支持
// （没有 json_each），所以对它筛选/排序会直接报错 —— fail-loud，而不是静默返回空。
// 需要按元素反查（例如"谁是 owner"）请用 refs。
type multiselectKind struct{}

// KindMultiselect kind 名（同时是后台组件文件名 widgets/multiselect.vue）。
const KindMultiselect = "multiselect"

func (multiselectKind) Name() string { return KindMultiselect }

// Validate 值 = 字符串数组，每项 ∈ options，不允许重复。
func (multiselectKind) Validate(f FieldDef, v any) error {
	var items []any
	switch list := v.(type) {
	case []any:
		items = list
	case []string:
		items = make([]any, len(list))
		for i, s := range list {
			items[i] = s
		}
	default:
		return fmt.Errorf("expects array of strings, got %T", v)
	}
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return fmt.Errorf("[%d] expects string, got %T", i, item)
		}
		if !slices.Contains(f.Options, s) {
			return fmt.Errorf("[%d] %q not in options %v", i, s, f.Options)
		}
		if seen[s] {
			return fmt.Errorf("[%d] duplicate option %q", i, s)
		}
		seen[s] = true
	}
	return nil
}

func (multiselectKind) IsEmpty(v any) bool {
	switch list := v.(type) {
	case []any:
		return len(list) == 0
	case []string:
		return len(list) == 0
	}
	return true
}

func (multiselectKind) Class() Class { return ClassField }

// QueryOps 空: 数组元素反查未实现（见文件头注释）。Query Compiler 据此拒绝筛选/排序。
func (multiselectKind) QueryOps() QueryOps { return QueryOps{} }

func (multiselectKind) ValidateField(t *Types, typeName string, f FieldDef, defs map[string]TypeDef) error {
	if err := rejectRefAttrs(typeName, f); err != nil {
		return err
	}
	if len(f.Options) == 0 {
		return fmt.Errorf("types: type %q field %q: multiselect requires options", typeName, f.Name)
	}
	seen := map[string]bool{}
	for _, o := range f.Options {
		if strings.TrimSpace(o) == "" {
			return fmt.Errorf("types: type %q field %q: empty option", typeName, f.Name)
		}
		if seen[o] {
			return fmt.Errorf("types: type %q field %q: duplicate option %q", typeName, f.Name, o)
		}
		seen[o] = true
	}
	return nil
}
