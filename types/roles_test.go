package types

import (
	"strings"
	"testing"
)

const authTypesYAML = `
types:
  member:
    capabilities:
      authentication: { roles: [秘书处, 审核员] }
    fields:
      - { name: name, kind: text }
`

// 注入：站点不写 roles 字段，词表来自 capability，系统角色自动并入。
func TestAuthInjectsRolesField(t *testing.T) {
	ts := New()
	if err := ts.Load([]byte(authTypesYAML)); err != nil {
		t.Fatal(err)
	}
	f, ok := ts.Field("member", "roles")
	if !ok {
		t.Fatal("roles 字段没被注入")
	}
	if f.Kind != KindMultiselect {
		t.Fatalf("roles kind = %q, want %q", f.Kind, KindMultiselect)
	}
	want := []string{RoleOwner, RoleAdmin, "秘书处", "审核员"}
	if len(f.Options) != len(want) {
		t.Fatalf("options = %v, want %v", f.Options, want)
	}
	for i := range want {
		if f.Options[i] != want[i] {
			t.Fatalf("options = %v, want %v", f.Options, want)
		}
	}
	// 词表就是值域：合法值通过, 词表外的值拒绝
	if err := ts.ValidateValue("member", f, []any{"秘书处", RoleAdmin}); err != nil {
		t.Fatalf("合法角色被拒: %v", err)
	}
	if err := ts.ValidateValue("member", f, []any{"自己编的"}); err == nil {
		t.Fatal("词表外的角色必须被拒")
	}
}

// 站点自己声明 roles 字段 ⇒ 报错（不静默覆盖）。
func TestAuthRejectsSiteDeclaredRolesField(t *testing.T) {
	ts := New()
	err := ts.Load([]byte(`
types:
  member:
    capabilities:
      authentication: { roles: [秘书处] }
    fields:
      - { name: roles, kind: test-no-such-kind }
`))
	if err == nil {
		t.Fatal("站点自带 roles 字段必须报错")
	}
}

// 词表问题一律加载期报错：空 / 重复 / 系统角色 / 空项 / 控制字符 / 超长。
func TestAuthRoleVocabularyValidation(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"empty", "    capabilities: { authentication: { roles: [] } }", "不能为空"},
		{"duplicate", "    capabilities: { authentication: { roles: [审核员, 审核员] } }", "重复"},
		{"system role", "    capabilities: { authentication: { roles: [owner] } }", "系统角色"},
		{"padded", "    capabilities: { authentication: { roles: [\" 审核员 \"] } }", "空白"},
		{"control char", "    capabilities: { authentication: { roles: [\"审\\n核\"] } }", "控制字符"},
		{"too long", "    capabilities: { authentication: { roles: [" + strings.Repeat("x", maxRoleNameLen+1) + "] } }", "超过"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := New()
			err := ts.Load([]byte("types:\n  member:\n" + c.yaml + "\n    fields: []\n"))
			if err == nil {
				t.Fatal("必须报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误 %q 不含 %q", err, c.want)
			}
		})
	}
}

// 没有 authentication 的类型不注入 roles；bool 旧写法不再接受（v0 不留兼容）。
func TestAuthOnlyInjectsForAuthTypes(t *testing.T) {
	ts := New()
	if err := ts.Load([]byte("types:\n  article:\n    fields: [{ name: title, kind: text }]\n")); err != nil {
		t.Fatal(err)
	}
	if _, ok := ts.Field("article", "roles"); ok {
		t.Fatal("非认证类型不该有 roles 字段")
	}
	ts2 := New()
	if err := ts2.Load([]byte("types:\n  member:\n    capabilities: { authentication: true }\n    fields: []\n")); err == nil {
		t.Fatal("authentication: true 已不再接受（要写 roles 词表）")
	}
}
