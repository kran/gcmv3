package types

import (
	"fmt"
	"strings"
)

// KindAddress 地址（URL 段）字段的类型。
const KindAddress = "address"

// AddressField 地址字段名 —— **由 addressable 能力注入**, 站点不写。
//
// 为什么是注入而不是"约定字段名"：注入的字段会让"选择性参与"变得**免费** ——
// 非 addressable 类型没有这个字段 ⇒ 存储层那个从它投影的生成列算出来是 NULL
// ⇒ 自动不进地址的唯一索引。于是既不用把类型名写进 DDL（改配置不必重建表），
// 又能让地址空间干净。
//
// 同样的形状见 authentication → roles：能力声明, 引擎注入, 站点手写报错。
const AddressField = "address"

type addressKind struct{}

func (addressKind) Name() string { return KindAddress }

func (addressKind) Validate(_ FieldDef, value any) error {
	address, ok := value.(string)
	if !ok {
		return fmt.Errorf("expects address string, got %T", value)
	}
	if address != "" && !ValidAddress(address) {
		return fmt.Errorf("invalid address %q", address)
	}
	return nil
}

func (addressKind) IsEmpty(value any) bool {
	address, ok := value.(string)
	return !ok || strings.TrimSpace(address) == ""
}

func (addressKind) Class() Class { return ClassField }
func (addressKind) QueryOps() QueryOps {
	return QueryOps{Equal: true, Text: true, Sortable: true}
}

func (addressKind) ValidateField(_ *Types, typeName string, field FieldDef, _ map[string]TypeDef) error {
	if field.Name != AddressField {
		return fmt.Errorf("types: type %q: kind %q is reserved for the %q field",
			typeName, field.Kind, AddressField)
	}
	return rejectRefAttrs(typeName, field)
}

// injectAddress 给声明了 addressable 的类型补上地址字段。
//
// 站点**写不了**这个字段: address 是节点保留列, 通用校验 (IsReservedField) 会先
// 把它挡住 —— 所以这里只管"该有的补上"。没有能力的类型自然就没有它 ⇒ 存储层那个
// 生成列算出来是 NULL ⇒ 不进地址的唯一索引（选择性参与是免费的）。
//
// 地址的**值**是普通标量字段, 所以授权/字段掩码/后台表单/读投影全都复用现成机制。
// "发布必须有地址"这类要求属于**业务规则**, 写在读层的写策略里, 不进 schema。
func injectAddress(typeName string, td *TypeDef) {
	if !td.Capabilities.Addressable {
		return
	}
	td.Fields = append(td.Fields, FieldDef{
		Name:  AddressField,
		Label: "地址",
		Kind:  KindAddress,
	})
}

// Addressable 该类型是否声明了 addressable 能力。
func (t *Types) Addressable(typeName string) bool {
	td, ok := t.defs[typeName]
	return ok && td.Capabilities.Addressable
}

// Address 该类型节点的地址值（不是 addressable 类型 ⇒ 空）。
func (t *Types) Address(typeName string, fields map[string]any) string {
	if !t.Addressable(typeName) {
		return ""
	}
	value, _ := fields[AddressField].(string)
	return value
}
