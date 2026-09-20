package types

import (
	"fmt"
	"strings"
	"testing"
)

// 合法完整定义: 文章↔专家双向引用、相关对称、分类树传递、归属、关系节点。
const validYAML = `
types:
  article:
    capabilities:
      searchable: { fields: [body] }
    fields:
      - { name: body, kind: richtext, required: true }
      - { name: cover, kind: upload-image }
      - { name: authors, kind: "refs", to: person }
      - { name: related, kind: "refs", to: article }
      - { name: categories, kind: "refs", to: category }
  category:
    fields:
      - { name: name, kind: text, required: true }
      - { name: parent, kind: ref, to: category, transitive: true }

      - { name: banner, kind: upload-image }
  person:
    fields:
      - { name: name, kind: text, required: true }
      - { name: articles, kind: "refs", to: article }
      - { name: employment, kind: "refs", to: employment }
  org:
    fields:
      - { name: name, kind: text, required: true }
  employment:
    fields:
      - { name: person, kind: ref, to: person, required: true }
      - { name: org, kind: ref, to: org, required: true }
      - { name: role, kind: text }
`

func TestLoadValid(t *testing.T) {
	ts := New()
	if err := ts.Load([]byte(validYAML)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	td, ok := ts.Type("article")
	if !ok {
		t.Fatal("article type missing")
	}
	if td.Capabilities.Searchable == nil {
		t.Fatalf("article searchable capability: %+v", td)
	}
	// 引用字段
	if _, ok := ts.Field("article", "related"); !ok {
		t.Fatal("related ref missing")
	}
	if f, ok := ts.Field("category", "parent"); !ok || !f.Transitive {
		t.Fatalf("parent relation metadata = %#v", f)
	}
	if f, ok := ts.Field("employment", "person"); !ok || !f.Required {
		t.Fatalf("required ref metadata = %#v", f)
	}
	if len(ts.Names()) != 5 {
		t.Fatalf("names: %v", ts.Names())
	}
}

// 非法定义表驱动: 每种错误类型一个用例, 必须 fail-loud。
func TestLoadInvalid(t *testing.T) {
	base := "types:\n  article:\n    fields:\n"
	cases := []struct {
		name string
		yaml string
		want string // 错误信息片段
	}{
		{"empty", "types: {}", "no types"},
		{"type name", base + "      - { name: body, kind: richtext }\n  BadType:\n    fields: []", "must match"},
		{"unknown kind", base + "      - { name: body, kind: banana }", "unknown kind"},
		{"bad field name", base + "      - { name: 'Bad-Name', kind: textarea }", "must match"},
		{"duplicate field", base + "      - { name: body, kind: textarea }\n      - { name: body, kind: textarea }", "duplicate"},
		{"ref no to", base + "      - { name: authors, kind: ref }", "requires to"},
		{"ref to undefined", base + "      - { name: authors, kind: ref, to: ghost }", "not defined"},
		{"transitive needs self ref",
			base + "      - { name: r, kind: \"refs\", to: other, transitive: true }\n  other:\n    fields: []",
			"transitive requires a self reference"},
	}
	// 未知配置必须 fail-loud，不能静默忽略拼写错误或已移除字段。
	ts := New()
	if err := ts.Load([]byte("types:\n  article:\n    url: /bad space/{slug}\n    fields: []")); err == nil || !strings.Contains(err.Error(), "field url not found") {
		t.Fatalf("unknown type property must fail: %v", err)
	}
	if err := ts.Load([]byte("types:\n  article:\n    fields:\n      - { name: body, kind: text, mystery: true }")); err == nil || !strings.Contains(err.Error(), "field mystery not found") {
		t.Fatalf("unknown field property must fail: %v", err)
	}
	for _, c := range cases {
		ts := New()
		err := ts.Load([]byte(c.yaml))
		if err == nil {
			t.Fatalf("must fail for %q", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("error %q does not contain %q", err, c.want)
		}
	}
}

func TestValidateValue(t *testing.T) {
	ts := New()
	if err := ts.Load([]byte(validYAML)); err != nil {
		t.Fatal(err)
	}
	field := func(typ, name string) FieldDef {
		f, ok := ts.Field(typ, name)
		if !ok {
			t.Fatalf("field %s.%s missing", typ, name)
		}
		return f
	}
	// 合法
	okCases := []struct {
		typ, name string
		v         any
	}{
		{"article", "body", "<p>hi</p>"},
		{"article", "cover", "/uploads/x.png"},
		{"article", "authors", []any{int64(1), int64(2)}},
		{"employment", "person", int64(3)},
	}
	for _, c := range okCases {
		if err := ts.ValidateValue(c.typ, field(c.typ, c.name), c.v); err != nil {
			t.Fatalf("%s.%s=%v: %v", c.typ, c.name, c.v, err)
		}
	}
	// 非法
	badCases := []struct {
		typ, name string
		v         any
		want      string
	}{
		{"article", "body", 123, "expects string"},
		{"article", "authors", "not-array", "expects array"},
		{"article", "authors", []any{"str"}, "expects node id"},
		{"article", "authors", []any{1.5}, "integer"},
		{"employment", "person", "x", "expects node id"},
	}
	for _, c := range badCases {
		err := ts.ValidateValue(c.typ, field(c.typ, c.name), c.v)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s.%s=%v: err=%v want %q", c.typ, c.name, c.v, err, c.want)
		}
	}
}

// ValidateFields 整组校验: 未知字段拒绝 + required 检查。
func TestValidateFields(t *testing.T) {
	ts := New()
	_ = ts.Load([]byte(validYAML))
	// 未知字段
	err := ts.ValidateFields("article", map[string]any{"body": "x", "ghost": 1})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field must fail: %v", err)
	}
	// required 缺失
	err = ts.ValidateFields("article", map[string]any{"cover": "/x.png"})
	if err == nil || !strings.Contains(err.Error(), "required field") {
		t.Fatalf("required must fail: %v", err)
	}
	// required ref 缺失
	err = ts.ValidateFields("employment", map[string]any{"role": "秘书长"})
	if err == nil || !strings.Contains(err.Error(), "required field") {
		t.Fatalf("required ref must fail: %v", err)
	}
	// 合法
	if err := ts.ValidateFields("article", map[string]any{"body": "x"}); err != nil {
		t.Fatalf("valid: %v", err)
	}
}

// ── 站点扩展: RegisterKind ─────────────────────────

// dateKind 站点自定义 kind 示例: **只有日期**（YYYYMMDD 整数）—— 内置的 timestamp 是
// 时间点（Unix 秒）, 这个例子说明"站点可以自己定义一种值形态 + 复用内置控件"。
type dateKind struct{}

func (dateKind) Name() string { return "date" }
func (dateKind) Validate(_ FieldDef, v any) error {
	seconds, ok := UnixSeconds(v)
	if !ok {
		return fmt.Errorf("expects YYYYMMDD integer, got %T", v)
	}
	if seconds < 19000101 || seconds > 29991231 {
		return fmt.Errorf("invalid date %d (expects YYYYMMDD)", seconds)
	}
	month := seconds / 100 % 100
	day := seconds % 100
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return fmt.Errorf("invalid date %d (expects YYYYMMDD)", seconds)
	}
	return nil
}
func (dateKind) IsEmpty(v any) bool {
	seconds, ok := UnixSeconds(v)
	return !ok || seconds == 0
}
func (dateKind) Class() Class { return ClassField }

// 自定义 kind 复用内置渲染器（datetime）—— 前端零代码。
func (dateKind) QueryOps() QueryOps {
	return QueryOps{Equal: true, Ordered: true, Sortable: true}
}
func (dateKind) ValidateField(t *Types, typeName string, f FieldDef, defs map[string]TypeDef) error {
	return rejectRefAttrs(typeName, f)
}

func TestRegisterKind(t *testing.T) {
	ts := New()
	ts.RegisterKind(dateKind{})
	if _, ok := ts.Kind("date"); !ok {
		t.Fatal("date kind missing")
	}
	// 使用自定义 kind 的类型定义
	cfg := "types:\n  employment:\n    fields:\n      - { name: start_date, kind: date, required: true }\n"
	if err := ts.Load([]byte(cfg)); err != nil {
		t.Fatalf("Load with custom kind: %v", err)
	}
	f, _ := ts.Field("employment", "start_date")
	operations := ts.FieldQueryOps(f)
	if !operations.Equal || !operations.Ordered || !operations.Sortable || operations.Text {
		t.Fatalf("custom date query operations = %#v", operations)
	}
	if err := ts.ValidateValue("employment", f, int64(20240115)); err != nil {
		t.Fatalf("valid date: %v", err)
	}
	for _, bad := range []any{"2024-01-15", int64(20241301), int64(20240132), 0} {
		if err := ts.ValidateValue("employment", f, bad); err == nil {
			t.Fatalf("%#v 应当被拒（只收 YYYYMMDD 整数）", bad)
		}
	}
	// required 检查走自定义 IsEmpty
	if err := ts.ValidateFields("employment", map[string]any{}); err == nil ||
		!strings.Contains(err.Error(), "required field") {
		t.Fatalf("required date must fail: %v", err)
	}
}

// 未注册 kind 的容器: Load 必须 fail-loud。
func TestLoadUnknownKind(t *testing.T) {
	ts := New() // 未注册 date
	cfg := "types:\n  x:\n    fields:\n      - { name: d, kind: date }\n"
	err := ts.Load([]byte(cfg))
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("unknown kind must fail: %v", err)
	}
}

// 重复注册 panic（fail-loud）。
func TestRegisterDuplicate(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate register must panic")
		}
	}()
	ts := New()
	ts.RegisterKind(textKind{})
}

// tree capability 校验：parent 必须是自引用；后台 tree view 依赖该能力。
func TestTreeParentHint(t *testing.T) {
	ts := New()
	if err := ts.Load([]byte(`
types:
  category:
    capabilities:
    admin: { view: tree, tree: parent }
    fields:
      - { name: name, kind: text }
      - { name: position, kind: number }
      - { name: parent, kind: ref, to: category }
`)); err != nil {
		t.Fatalf("valid tree: %v", err)
	}
	if ts.TreeParent("category") != "parent" {
		t.Fatalf("admin.tree 应读到 parent, got %q", ts.TreeParent("category"))
	}
}

// title 穿透声明: 合法/非法校验。
// slug 约束: 字母开头 / 白名单字符 / 禁止连续 --。
func TestValidAddress(t *testing.T) {
	valid := []string{"ai", "ai-industry", "page1", "a_b", "a-1-b", "A-B"}
	invalid := []string{"", "1abc", "-abc", "_abc", "a--b", "a b", "a/b", "a..b", "a--", "-"}
	for _, s := range valid {
		if !ValidAddress(s) {
			t.Fatalf("valid slug %q rejected", s)
		}
	}
	for _, s := range invalid {
		if ValidAddress(s) {
			t.Fatalf("invalid slug %q accepted", s)
		}
	}
}

// 复合字段（array/object）: 定义校验 + 值递归校验 — Kind 接口不动的验证。
func TestCompositeFields(t *testing.T) {
	raw := `
types:
  page:
    fields:
      - { name: title, kind: text }
      - { name: tags, kind: array, item: { kind: textarea } }
      - { name: nav, kind: array, item: { kind: object, fields:
            [ { name: label, kind: text }, { name: url, kind: text } ] } }
      - { name: meta, kind: object, fields: [ { name: og_title, kind: textarea } ] }
`
	ts := New()
	if err := ts.Load([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	// 值校验: 合法
	fields := map[string]any{
		"title": "t",
		"tags":  []any{"a", "b"},
		"nav":   []any{map[string]any{"label": "首页", "url": "/"}},
		"meta":  map[string]any{"og_title": "x"},
	}
	if err := ts.ValidateFields("page", fields); err != nil {
		t.Fatalf("valid composite: %v", err)
	}
	// 非法: 元素类型错 / 未知子字段
	if err := ts.ValidateFields("page", map[string]any{"tags": []any{"a", 5}}); err == nil {
		t.Fatal("tag element must be string")
	}
	if err := ts.ValidateFields("page", map[string]any{"meta": map[string]any{"ghost": 1}}); err == nil {
		t.Fatal("unknown sub-field must fail")
	}
	// 定义校验: array 缺 item / object 缺 fields / 深度超限
	if err := New().Load([]byte(`
types:
  bad1: { fields: [ { name: a, kind: array } ] }`)); err == nil {
		t.Fatal("array without item must fail")
	}
	if err := New().Load([]byte(`
types:
  bad2: { fields: [ { name: a, kind: object } ] }`)); err == nil {
		t.Fatal("object without fields must fail")
	}
}

// 复合结构内不允许引用: 嵌套 ref 落不了 Edge, 降级成 fields JSON 就只剩裸 ID。
// 定义错误必须在 Load 期 fail-loud, 不能等到写入时才暴露。
func TestCompositeRejectsRef(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		{
			name: "array item ref",
			base: "      - { name: members, kind: array, item: { kind: ref } }",
			want: "kind ref cannot be nested",
		},
		{
			name: "array item ref with to",
			base: "      - { name: members, kind: array, item: { kind: ref, to: person } }",
			want: "kind ref cannot be nested",
		},
		{
			name: "array item ref list",
			base: "      - { name: members, kind: array, item: { kind: 'refs', to: person } }",
			want: "kind refs cannot be nested",
		},
		{
			name: "object sub-field ref",
			base: "      - { name: meta, kind: object, fields: [ { name: lead, kind: ref, to: person } ] }",
			want: "kind ref cannot be nested",
		},
		{
			name: "nested object ref path",
			base: "      - { name: meta, kind: object, fields: [ { name: inner, kind: object, fields: [ { name: lead, kind: ref } ] } ] }",
			want: `field "meta.inner.lead"`,
		},
		{
			name: "array item unknown kind",
			base: "      - { name: members, kind: array, item: { kind: ghost } }",
			want: "unknown kind",
		},
		{
			name: "array declares to",
			base: "      - { name: members, kind: array, to: person, item: { kind: text } }",
			want: "must not declare to/transitive",
		},
		{
			name: "array declares fields",
			base: "      - { name: members, kind: array, item: { kind: text }, fields: [ { name: a, kind: text } ] }",
			want: "array must not declare fields",
		},
		{
			name: "object declares item",
			base: "      - { name: meta, kind: object, item: { kind: text }, fields: [ { name: a, kind: text } ] }",
			want: "object must not declare item",
		},
		{
			name: "object duplicate sub-field",
			base: "      - { name: meta, kind: object, fields: [ { name: a, kind: text }, { name: a, kind: text } ] }",
			want: "duplicate sub-field",
		},
		{
			name: "object sub-field select needs options",
			base: "      - { name: meta, kind: object, fields: [ { name: a, kind: select } ] }",
			want: "select requires options",
		},
		{
			name: "composite nesting depth",
			base: "      - { name: nav, kind: array, item: { kind: object, fields: [ { name: a, kind: array, item: { kind: object, fields: [ { name: b, kind: array, item: { kind: object, fields: [ { name: c, kind: text } ] } } ] } } ] } }",
			want: "nesting depth exceeds",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			yaml := "types:\n  person: { fields: [] }\n  team:\n    fields:\n" + test.base + "\n"
			err := New().Load([]byte(yaml))
			if err == nil {
				t.Fatal("definition must be rejected")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not contain %q", err, test.want)
			}
		})
	}
}

// cmx 语义补齐: strings 归一 + object 子字段 required + array 元素 required 忽略。
func TestCompositeCmxSemantics(t *testing.T) {
	raw := `
types:
  page:
    fields:
      - { name: tags, kind: strings }  # 简写 → array<string>（kind 改名后为 array<text>）
      - { name: meta, kind: object, fields:
            [ { name: og_title, kind: text, required: true } ] }
`
	ts := New()
	if err := ts.Load([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	// strings 归一: kind 变 array
	td, _ := ts.Type("page")
	if td.Fields[0].Kind != "array" || td.Fields[0].Item == nil || td.Fields[0].Item.Kind != "text" {
		t.Fatalf("strings must normalize to array<text>: %+v", td.Fields[0])
	}
	// object 子字段 required 缺 → 拒绝
	if err := ts.ValidateFields("page", map[string]any{"tags": []any{"a"}, "meta": map[string]any{}}); err == nil {
		t.Fatal("required sub-field must be enforced")
	}
	// 合法
	if err := ts.ValidateFields("page", map[string]any{"tags": []any{"a"}, "meta": map[string]any{"og_title": "x"}}); err != nil {
		t.Fatalf("valid: %v", err)
	}
}

// select kind: options 必填/去重; 值必须在 options 内。
func TestCapabilitiesDefaultsAndAdmin(t *testing.T) {
	ts := New()
	err := ts.Load([]byte(`
types:
  article:
    capabilities:
      searchable: { fields: [title] }
      addressable: true
    admin: { view: list, label: 文章, group: 内容, columns: [address, state, updated_at] }
    fields:
      - { name: title, kind: text, required: true }
      - { name: state, kind: select, options: [draft, published], default: draft, required: true }
      - { name: position, kind: number, default: 0 }
      - { name: external_id, kind: text }
`))
	if err != nil {
		t.Fatal(err)
	}
	fields, err := ts.ApplyDefaults("article", map[string]any{"title": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if fields["state"] != "draft" || fields["position"] != 0 {
		t.Fatalf("defaults = %#v", fields)
	}
	if err := ts.ValidateFields("article", fields); err != nil {
		t.Fatal(err)
	}
	// 可写性归规则（web 层）, 类型声明里不再有 immutable 这种静态开关
	if err := ts.ValidatePatchFields("article", map[string]any{"external_id": "changed"}); err != nil {
		t.Fatalf("patch 一个普通字段该通过: %v", err)
	}
	// admin 段的后台展示名与分组（空 = 由后台回退成类型名/排在最前）
	admin := ts.Defs()["article"].Admin
	if admin.Label != "文章" || admin.Group != "内容" || admin.View != "list" {
		t.Fatalf("admin = %#v", admin)
	}
	if empty := ts.Defs()["article"].Admin.Columns; len(empty) != 3 {
		t.Fatalf("admin columns = %#v", empty)
	}
	// 地址字段由能力注入（站点没写）, 值是普通标量字段
	if _, ok := ts.Field("article", AddressField); !ok {
		t.Fatal("addressable 类型必须有注入的 address 字段")
	}
	if got := ts.Address("article", map[string]any{AddressField: "hello"}); got != "hello" {
		t.Fatalf("address = %q", got)
	}
	if got := ts.Address("nonexistent", nil); got != "" {
		t.Fatalf("不是 addressable 的类型没有地址: %q", got)
	}
}

// 地址字段: 能力注入, 站点手写报错; 没有能力的类型也不许有这个名字。
func TestAddressInjection(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"站点手写 address（连 addressable 类型也不许）",
			`types: { a: { capabilities: { addressable: true }, fields: [ { name: address, kind: address } ] } }`,
			"reserved for node columns"},
		{"非 addressable 类型声明 address",
			`types: { a: { fields: [ { name: address, kind: address } ] } }`,
			"reserved for node columns"},
		{"kind address 只能叫 address",
			`types: { a: { capabilities: { addressable: true }, fields: [ { name: other, kind: address } ] } }`,
			"reserved for the"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := New().Load([]byte(test.yaml))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}

	// 地址格式校验走注入字段的 kind
	ts := New()
	err := ts.Load([]byte(`types: { a: { capabilities: { addressable: true }, fields: [ { name: t, kind: text } ] } }`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.ValidateFields("a", map[string]any{"t": "x", "address": "not a slug!"}); err == nil {
		t.Fatal("非法地址必须被拒")
	}
	if err := ts.ValidateFields("a", map[string]any{"t": "x", "address": "ok-slug"}); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := New().Load([]byte(test.yaml))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

// constraints 这个机制已删除（唯一性/索引一律写在迁移的静态 DDL 里）——
// 站点 yaml 里还写着它必须**响亮地报错**, 不能静默忽略。
func TestConstraintsMechanismGone(t *testing.T) {
	err := New().Load([]byte(`types: { a: { constraints: { unique: [[x]] }, fields: [ { name: x, kind: text } ] } }`))
	if err == nil {
		t.Fatal("constraints 已删除, 写了必须报错（不能静默忽略）")
	}
	if !strings.Contains(err.Error(), "constraints") {
		t.Fatalf("错误信息要点出是哪个键: %v", err)
	}
}

func TestSelectKind(t *testing.T) {
	ts := New()
	// 无 options 拒绝
	if err := ts.Load([]byte("types:\n  x:\n    fields:\n      - { name: t, kind: select }\n")); err == nil {
		t.Fatal("select 无 options 应拒绝")
	}
	// 合法
	ts2 := New()
	if err := ts2.Load([]byte("types:\n  x:\n    fields:\n      - { name: t, kind: select, options: [a, b] }\n")); err != nil {
		t.Fatal(err)
	}
	// 值在 options 内 ✓
	if err := ts2.ValidateFields("x", map[string]any{"t": "a"}); err != nil {
		t.Fatal(err)
	}
	// 值不在 options 内 ✗
	if err := ts2.ValidateFields("x", map[string]any{"t": "c"}); err == nil {
		t.Fatal("值不在 options 应拒绝")
	}
}
