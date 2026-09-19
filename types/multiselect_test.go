package types

import (
	"strings"
	"testing"
)

func multiselectField() FieldDef {
	return FieldDef{Name: "roles", Kind: KindMultiselect, Options: []string{"a", "b", "c"}}
}

func TestMultiselectValidate(t *testing.T) {
	f := multiselectField()
	// 注意: nil 不是合法值（与 select 一致：显式设为 null 会报错; required 的判空走 IsEmpty）
	ok := []any{
		[]any{"a"},
		[]any{"a", "c"},
		[]string{"b"},
		[]any{}, // 空数组 = 没选（required 由 IsEmpty 管）
	}
	for _, v := range ok {
		if err := (multiselectKind{}).Validate(f, v); err != nil {
			t.Fatalf("Validate(%#v) = %v", v, err)
		}
	}
	bad := []struct {
		v    any
		want string
	}{
		{"a", "expects array of strings"},
		{[]any{1}, "expects string"},
		{[]any{"z"}, "not in options"},
		{[]any{"a", "a"}, "duplicate option"},
	}
	for _, c := range bad {
		err := (multiselectKind{}).Validate(f, c.v)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("Validate(%#v) = %v, want 含 %q", c.v, err, c.want)
		}
	}
}

func TestMultiselectIsEmpty(t *testing.T) {
	k := multiselectKind{}
	for _, v := range []any{nil, []any{}, []string{}} {
		if !k.IsEmpty(v) {
			t.Fatalf("IsEmpty(%#v) = false", v)
		}
	}
	if k.IsEmpty([]any{"a"}) {
		t.Fatal("非空数组不该是 empty")
	}
}

// 查询能力必须为空：数组元素反查未实现 ⇒ 筛选/排序要 fail-loud 而不是静默返回空。
func TestMultiselectHasNoQueryOps(t *testing.T) {
	got := (multiselectKind{}).QueryOps()
	if got.Equal || got.Ordered || got.Text || got.Sortable {
		t.Fatalf("multiselect 不该声明任何查询能力: %#v", got)
	}
}

func TestMultiselectValidateField(t *testing.T) {
	ts := New()
	cases := []struct {
		name string
		f    FieldDef
		want string
	}{
		{"no options", FieldDef{Name: "roles", Kind: KindMultiselect}, "requires options"},
		{"dup option", FieldDef{Name: "roles", Kind: KindMultiselect, Options: []string{"a", "a"}}, "duplicate option"},
		{"empty option", FieldDef{Name: "roles", Kind: KindMultiselect, Options: []string{"a", " "}}, "empty option"},
	}
	for _, c := range cases {
		err := (multiselectKind{}).ValidateField(ts, "member", c.f, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want 含 %q", c.name, err, c.want)
		}
	}
}
