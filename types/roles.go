package types

import (
	"fmt"
	"sort"
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

// PasswordMethods 框架内置的**口令类**登录方式（不声明 authentication.methods 时的默认）。
//
// 三者对框架是一回事: 凭据里 data.password 存在就能登录（identifier 分别是邮箱/手机号/
// 用户名 —— 框架不做格式校验, 它不解释标识）。
var PasswordMethods = []string{"email", "phone", "username"}

// AuthenticationCapability 认证能力：该类型可以登录。
//
// 两个词表都是**唯一声明处**, 打错一个字母不静默:
//
//	Roles    角色词表（权限映射 grant 里引用的角色必须出现在这里）
//	Methods  框架管理的**口令类**登录方式（不写 = PasswordMethods）
//
// Methods 只管"框架能写/能核验"的那些: 口令类的凭据写入（注册/绑定/后台设置）
// 与口令登录都按它校验。插件自己的登录机制（微信/SMS/SSO）**不列进来** ——
// 那些凭据由插件自己写、自己核验, 框架只负责列出与解绑（口径: "口令内置,
// 插件只服务站点特有的外部身份源"）。
type AuthenticationCapability struct {
	Roles   []string `yaml:"roles" json:"roles"`
	Methods []string `yaml:"methods,omitempty" json:"methods,omitempty"`
}

// AuthMethods 该类型**框架管理的口令类登录方式**（已并入默认值）。
//
// 第二个返回值 = 这个类型能不能登录（声明了 authentication 能力）。
func (t *Types) AuthMethods(typeName string) ([]string, bool) {
	td, ok := t.defs[typeName]
	if !ok || td.Capabilities.Authentication == nil {
		return nil, false
	}
	declared := td.Capabilities.Authentication.Methods
	if len(declared) == 0 {
		out := make([]string, len(PasswordMethods))
		copy(out, PasswordMethods)
		return out, true
	}
	out := make([]string, len(declared))
	copy(out, declared)
	return out, true
}

// HasAuthMethod 该类型是否声明了某个口令类方式。
func (t *Types) HasAuthMethod(typeName, method string) bool {
	methods, ok := t.AuthMethods(typeName)
	if !ok {
		return false
	}
	for _, name := range methods {
		if name == method {
			return true
		}
	}
	return false
}

// AuthTypes 声明了 authentication 能力的类型名（字典序稳定输出）。
func (t *Types) AuthTypes() []string {
	out := []string{}
	for name, td := range t.defs {
		if td.Capabilities.Authentication != nil {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
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
		if err := validateAuthMethods(typeName, capability.Methods); err != nil {
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

// validateAuthMethods 口令类方式校验：无空项/空白、无重复、长度上限。
//
// 空 = 用默认（PasswordMethods），不是错误。
func validateAuthMethods(typeName string, methods []string) error {
	seen := make(map[string]bool, len(methods))
	for _, method := range methods {
		switch {
		case strings.TrimSpace(method) != method || method == "":
			return fmt.Errorf("types: type %q: authentication.methods 里的 %q 不合法（空或含首尾空白）", typeName, method)
		case strings.ContainsAny(method, " \n\r\t"):
			return fmt.Errorf("types: type %q: authentication.methods 里的 %q 含空白/控制字符", typeName, method)
		case len([]rune(method)) > maxRoleNameLen:
			return fmt.Errorf("types: type %q: authentication.methods 里的 %q 超过 %d 个字符", typeName, method, maxRoleNameLen)
		case seen[method]:
			return fmt.Errorf("types: type %q: authentication.methods 里的 %q 重复", typeName, method)
		}
		seen[method] = true
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
