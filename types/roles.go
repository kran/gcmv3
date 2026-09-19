package types

import (
	"fmt"
	"strings"
)

// 系统角色：内核保留，自动补进每个声明了 authentication 的词表。
//
//	owner —— 字段通配 "*"（含 roles 字段 ⇒ 只有它能授角色）
//	admin —— 只给"进入系统后台"这道门，没有任何字段权限
//
// 判定与求值规则见 docs/todo-role-permissions.md。
const (
	RoleOwner = "owner"
	RoleAdmin = "admin"
	// RolePublic 「人人都有」的基础角色（匿名 + 已登录都有）—— 公开表单（留言板、
	// 联系我们）的规则授予它；它不进任何词表，也不给后台权限。
	//
	// 已认证的节点身份用**它自己的类型名**当角色（如 "member" / "staff"）——
	// 系统支持多种 auth 类型，所以"是不是会员"不该是一个通用角色，而是类型本身；
	// "会员只能改自己的"= g.Allow(<类型名>, …) + 回调里的归属判断。
	RolePublic = "public"

	// RolesField 系统自动注入的字段名（站点不得自己声明）。
	RolesField = "roles"

	// maxRoleNameLen 角色名长度上限（runes）。
	maxRoleNameLen = 32
)

// systemRoles 内核保留角色（顺序稳定，避免 options 抖动）。
func systemRoles() []string { return []string{RoleOwner, RoleAdmin} }

// AuthenticationCapability 认证能力：该类型可以登录，角色词表见 Roles。
//
// 词表是角色名的**唯一声明处** —— 权限映射（grant）里引用的角色必须出现在这里，
// 否则加载期报错（打错一个字母不能静默变成"没人有权限"）。
type AuthenticationCapability struct {
	Roles []string `yaml:"roles" json:"roles"`
}

// injectAuthRoles 给声明了 authentication 的类型自动注入 roles 字段。
//
// 站点**不写**这个字段：词表来自 capabilities.authentication.roles，系统角色自动并入。
// 站点自己声明了同名字段 ⇒ 报错（不静默覆盖）。
func injectAuthRoles(defs map[string]TypeDef) error {
	for typeName, td := range defs {
		capability := td.Capabilities.Authentication
		if capability == nil {
			continue
		}
		if err := validateAuthRoles(typeName, capability.Roles); err != nil {
			return err
		}
		for _, role := range capability.Roles {
			if _, clash := defs[role]; clash {
				return fmt.Errorf("types: type %q: 角色名 %q 与类型名相同 —— 已认证身份按**类型名**授予角色，会撞车", typeName, role)
			}
		}
		for _, f := range td.Fields {
			if f.Name == RolesField {
				return fmt.Errorf(
					"types: type %q: field %q 由 authentication capability 自动注入，删掉声明（词表写在 capabilities.authentication.roles）",
					typeName, RolesField)
			}
		}
		td.Fields = append(td.Fields, FieldDef{
			Name:    RolesField,
			Kind:    KindMultiselect,
			Label:   "角色",
			Options: append(systemRoles(), capability.Roles...),
		})
		defs[typeName] = td
	}
	return nil
}

// validateAuthRoles 词表校验：非空、无空项/空白、无重复、不得重复声明系统角色。
func validateAuthRoles(typeName string, roles []string) error {
	if len(roles) == 0 {
		return fmt.Errorf("types: type %q: capabilities.authentication.roles 不能为空（能登录却不可能有任何角色是无意义的配置）", typeName)
	}
	seen := make(map[string]bool, len(roles))
	for _, role := range roles {
		switch {
		case strings.TrimSpace(role) != role || role == "":
			return fmt.Errorf("types: type %q: 角色名 %q 不合法（空或含首尾空白）", typeName, role)
		case strings.ContainsAny(role, "\n\r\t"):
			return fmt.Errorf("types: type %q: 角色名 %q 含控制字符", typeName, role)
		case len([]rune(role)) > maxRoleNameLen:
			return fmt.Errorf("types: type %q: 角色名 %q 超过 %d 个字符", typeName, role, maxRoleNameLen)
		case role == RoleOwner || role == RoleAdmin:
			return fmt.Errorf("types: type %q: 角色 %q 是系统角色（自动加入，不用声明）", typeName, role)
		case seen[role]:
			return fmt.Errorf("types: type %q: 角色 %q 重复", typeName, role)
		}
		seen[role] = true
	}
	return nil
}
