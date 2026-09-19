package types

import (
	"fmt"
	"maps"
)

const (
	AddressUniqueGlobal = "global"
	AdminViewList       = "list"
	AdminViewTree       = "tree"
)

// ApplyDefaults 返回字段副本，并只为创建时缺失的字段应用默认值。
func (t *Types) ApplyDefaults(typeName string, fields map[string]any) (map[string]any, error) {
	td, ok := t.defs[typeName]
	if !ok {
		return nil, fmt.Errorf("types: type %q not defined", typeName)
	}
	out := make(map[string]any, len(fields)+len(td.Fields))
	maps.Copy(out, fields)
	for _, field := range td.Fields {
		if _, exists := out[field.Name]; !exists && field.Default != nil {
			out[field.Name] = cloneValue(field.Default)
		}
	}
	return out, nil
}

// Searchable 返回 searchable 配置。
func (t *Types) Searchable(typeName string) (SearchableCapability, bool) {
	td, ok := t.defs[typeName]
	if !ok || td.Capabilities.Searchable == nil {
		return SearchableCapability{}, false
	}
	return *td.Capabilities.Searchable, true
}

// Tree 返回 tree 配置。
func (t *Types) validateTypeConfig(typeName string, td TypeDef) error {
	field := func(name string) (FieldDef, error) {
		f, ok := FieldByName(td, name)
		if !ok {
			return FieldDef{}, fmt.Errorf("types: type %q: field %q is not defined", typeName, name)
		}
		return f, nil
	}

	if searchable := td.Capabilities.Searchable; searchable != nil {
		if len(searchable.Fields) == 0 {
			return fmt.Errorf("types: type %q: searchable.fields required", typeName)
		}
		// 列进来会走通用校验 "field not defined"。
		for _, name := range searchable.Fields {
			f, err := field(name)
			if err != nil {
				return err
			}
			if !t.FieldQueryOps(f).Text {
				return fmt.Errorf("types: type %q: searchable field %q must support text queries", typeName, name)
			}
		}
	}

	view := td.Admin.View
	if view != "" && view != AdminViewList && view != AdminViewTree {
		return fmt.Errorf("types: type %q: admin.view must be list or tree", typeName)
	}
	if view == AdminViewTree {
		// 树视图是**展示**：需要明确指出哪个字段当父（不做推导 —— 自引用 ref 也可能是"相关"）
		if td.Admin.Tree == "" {
			return fmt.Errorf("types: type %q: admin.view=tree requires admin.tree (parent field)", typeName)
		}
		if _, err := field(td.Admin.Tree); err != nil {
			return err
		}
	}
	for _, name := range td.Admin.Columns {
		if IsNodeColumn(name) && name != "fields" {
			continue
		}
		if _, err := field(name); err != nil {
			return err
		}
	}
	return nil
}

func cloneValue(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = cloneValue(v[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = cloneValue(item)
		}
		return out
	default:
		return value
	}
}
