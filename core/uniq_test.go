package core

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kran/dba"
	"github.com/kran/gcmv3/types"
	_ "modernc.org/sqlite"
)

const uniqTypesYAML = `
types:
  member:
    capabilities: { unique: [credit_code] }
    fields:
      - { name: name, kind: text }
      - { name: credit_code, kind: text }
  organization:
    capabilities: { unique: [credit_code] }
    fields:
      - { name: name, kind: text }
      - { name: credit_code, kind: text }
  person:
    capabilities: { addressable: true }
    fields:
      - { name: name, kind: text }
  company:
    capabilities: { addressable: true }
    fields:
      - { name: name, kind: text }
  position:
    capabilities: { unique: [person, company] }
    fields:
      - { name: person, kind: ref, to: person }
      - { name: company, kind: ref, to: company }
      - { name: title, kind: text }
`

func uniqFixture(t *testing.T) *GCM {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "gcm.sqlite") +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := dba.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	applySchema(t, db)
	ts := types.New()
	err = ts.Load([]byte(uniqTypesYAML))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := OpenGCM(db, ts)
	if err != nil {
		t.Fatal(err)
	}
	return gcm
}

// 标量唯一: 同类型内撞 ⇒ ErrDuplicate(2067); 空值放行; 跨类型放行。
func TestUniqueScalarWithinType(t *testing.T) {
	gcm := uniqFixture(t)
	first, err := gcm.CreateNode(nil, &Node{Type: "member", Fields: Fields{"name": "甲", "credit_code": "A"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = gcm.CreateNode(nil, &Node{Type: "member", Fields: Fields{"name": "乙", "credit_code": "A"}})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("同类型同值该 ErrDuplicate, 实际 %v", err)
	}
	// 另一个类型用同一个值 —— 类型内唯一 ⇒ 必须放行
	_, err = gcm.CreateNode(nil, &Node{Type: "organization", Fields: Fields{"name": "某公司", "credit_code": "A"}})
	if err != nil {
		t.Fatalf("跨类型该放行: %v", err)
	}
	// 空值不参与唯一
	for i, name := range []string{"无码一", "无码二"} {
		_, err = gcm.CreateNode(nil, &Node{Type: "member", Fields: Fields{"name": name}})
		if err != nil {
			t.Fatalf("第 %d 条空值该放行: %v", i+1, err)
		}
	}
	_ = first
}

// 引用元组唯一: (人, 公司) 撞 ⇒ ErrDuplicate; 换一个 ⇒ 放行; 更新沿用/改值/改空。
func TestUniqueRefTuple(t *testing.T) {
	gcm := uniqFixture(t)
	person, err := gcm.CreateNode(nil, &Node{Type: "person", Fields: Fields{"name": "张三"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = gcm.CreateNode(nil, &Node{Type: "person", Fields: Fields{"name": "李四"}})
	if err != nil {
		t.Fatal(err)
	}
	company, err := gcm.CreateNode(nil, &Node{Type: "company", Fields: Fields{"name": "恒新"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := gcm.CreateNode(nil, &Node{Type: "company", Fields: Fields{"name": "纵横"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = gcm.CreateNode(nil, &Node{Type: "position", Fields: Fields{
		"person": person, "company": company, "title": "董事长"}})
	if err != nil {
		t.Fatal(err)
	}
	// 同一个人 + 同一家公司 ⇒ 撞
	_, err = gcm.CreateNode(nil, &Node{Type: "position", Fields: Fields{
		"person": person, "company": company, "title": "总经理"}})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("同元组该 ErrDuplicate, 实际 %v", err)
	}
	// 换公司 ⇒ 放行（元组不同）
	id, err := gcm.CreateNode(nil, &Node{Type: "position", Fields: Fields{
		"person": person, "company": second, "title": "总经理"}})
	if err != nil {
		t.Fatalf("不同元组该放行: %v", err)
	}

	// 改**别的**字段（标题）: 引用没动 ⇒ 键不变 ⇒ 不能误判为冲突
	err = gcm.PatchNode(nil, id, &NodePatch{Revision: ptr64(1), Fields: Fields{"title": "副董事长"}})
	if err != nil {
		t.Fatalf("改标题不该冲突（键里的引用没动）: %v", err)
	}
	// 改公司 ⇒ 变成已存在的 (person, company) ⇒ 撞
	err = gcm.PatchNode(nil, id, &NodePatch{Revision: ptr64(2), Fields: Fields{"company": company}})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("改成已存在的元组该 ErrDuplicate, 实际 %v", err)
	}
	// 引用置空 ⇒ 整键 NULL ⇒ 不再参与唯一
	err = gcm.PatchNode(nil, id, &NodePatch{Revision: ptr64(2), Fields: Fields{"company": nil}})
	if err != nil {
		t.Fatalf("置空该放行: %v", err)
	}
}

// 声明校验: 多值/复合字段不能当唯一键; 重复声明报错。
func TestUniqueCapabilityValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"refs 当唯一键", `
types:
  member:
    capabilities: { unique: [tags] }
    fields:
      - { name: tags, kind: "refs", to: member }
`, "不能当唯一键"},
		{"字段不存在", `
types:
  member:
    capabilities: { unique: [nope] }
    fields:
      - { name: name, kind: text }
`, "not defined"},
		{"重复声明", `
types:
  member:
    capabilities: { unique: [code, code] }
    fields:
      - { name: code, kind: text }
`, "重复"},
	}
	for _, c := range cases {
		ts := types.New()
		err := ts.Load([]byte(c.yaml))
		if err == nil {
			t.Fatalf("%s: 该报错", c.name)
		}
		if !contains(err.Error(), c.want) {
			t.Fatalf("%s: 错误信息该提到 %q, 实际 %v", c.name, c.want, err)
		}
	}
}

func ptr64(v int64) *int64 { return &v }

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

// 引用只收 id（地址当场拒）⇒ 投影可以直接拿字段视图, 不需要归一化那一步。
//
// 这条测试是"为什么不需要 split 之后的值"的证据: 能进到投影的引用值只可能是 id。
func TestUniqueRefNormalizesAddressAndID(t *testing.T) {
	gcm := uniqFixture(t)
	person, err := gcm.CreateNode(nil, &Node{Type: "person", Fields: Fields{
		"name": "张三", "address": "zhangsan"}})
	if err != nil {
		t.Fatal(err)
	}
	company, err := gcm.CreateNode(nil, &Node{Type: "company", Fields: Fields{
		"name": "恒新", "address": "hengxin"}})
	if err != nil {
		t.Fatal(err)
	}
	// 第一条: 用 **id**
	_, err = gcm.CreateNode(nil, &Node{Type: "position", Fields: Fields{
		"person": person, "company": company, "title": "董事长"}})
	if err != nil {
		t.Fatal(err)
	}
	// 引用**只能是 id**: 写地址被字段校验当场拒（不静默 —— 否则键里会混进地址,
	// 同一个目标两种写法算出两个键, 唯一约束形同虚设）
	_, err = gcm.CreateNode(nil, &Node{Type: "position", Fields: Fields{
		"person": "zhangsan", "company": "hengxin", "title": "总经理"}})
	if err == nil {
		t.Fatal("引用写地址该被拒")
	}
	if !strings.Contains(err.Error(), "expects node id") {
		t.Fatalf("错误信息该点明只收 id: %v", err)
	}
}
