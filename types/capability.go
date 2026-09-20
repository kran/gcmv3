package types

import (
	"fmt"
	"maps"
	"sort"
	"strings"
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

	for _, name := range td.Capabilities.Unique {
		f, err := field(name)
		if err != nil {
			return err
		}
		// 唯一键的每一部分必须是**标量或单引用**: 多值引用（refs）是集合,
		// 没有规范顺序 ⇒ "两个集合相等"的语义不清, 当场拒（不猜）。
		if f.Kind == KindRefList || f.Kind == KindArray || f.Kind == KindObject {
			return fmt.Errorf("types: type %q: capabilities.unique 里的 %q（kind %s）不能当唯一键 —— "+
				"多值/复合字段没有规范顺序", typeName, name, f.Kind)
		}
	}
	if duplicate := duplicateName(td.Capabilities.Unique); duplicate != "" {
		return fmt.Errorf("types: type %q: capabilities.unique 里 %q 重复", typeName, duplicate)
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
	if td.Admin.Display != "" {
		// 显示名字段必须是**文本类标量**: 指向一张图片/一个引用的话, 界面上就是
		// 一串路径或一个对象 —— 那是配置错, 当场报, 别等到界面上一片 #id 才发现。
		def, err := field(td.Admin.Display)
		if err != nil {
			return err
		}
		if !displayNameKinds[def.Kind] {
			return fmt.Errorf("types: type %q: admin.display = %q (kind %s) 不能当显示名 —— "+
				"只有文本类字段可以（%s）", typeName, td.Admin.Display, def.Kind, displayNameKindList())
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

// displayNameKinds 能当显示名的 kind。
//
// 为什么不用 QueryOps.Text 判定: select 的值也是字符串、也能当显示名, 但它不该被
// contains 检索（那是它没有 Text 能力的原因）—— 两件事别混。
var displayNameKinds = map[string]bool{
	KindText:     true,
	KindTextarea: true,
	KindSelect:   true,
	KindAddress:  true,
	KindRichtext: true,
}

func displayNameKindList() string {
	kinds := make([]string, 0, len(displayNameKinds))
	for kind := range displayNameKinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return strings.Join(kinds, " / ")
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

// duplicateName 返回第一个重复项（没有则空串）。
func duplicateName(names []string) string {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			return name
		}
		seen[name] = true
	}
	return ""
}
