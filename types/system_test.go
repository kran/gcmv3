package types

import (
	"strings"
	"testing"
	"time"
)

// 时间列的查询值: 只收 Unix 秒 —— 字符串/time.Time 会变成 "TEXT vs INTEGER" 比较
// （SQLite 里 TEXT 恒大于 INTEGER）, 静默给出错误结果, 所以在这里当场拒。
func TestTimeColumnQueryValues(t *testing.T) {
	ts := New()
	field, ok := ts.SystemField("updated_at")
	if !ok {
		t.Fatal("没有 updated_at")
	}
	if err := field.Validate(int64(1789000000)); err != nil {
		t.Fatalf("整数秒应当通过: %v", err)
	}
	for _, bad := range []any{
		"2026-09-14T23:06:41Z", // v2 的 ISO 表示
		time.Now(),             // time.Time（要调用方自己 .Unix()）
		1789000000.5,
		nil,
	} {
		if err := field.Validate(bad); err == nil {
			t.Fatalf("%#v 应当被拒（时间列查询值只收整数秒）", bad)
		}
	}
}

// admin.display: 必须是声明过的、且能当显示名的字段（文本类标量）。
func TestAdminDisplayField(t *testing.T) {
	load := func(display string) error {
		ts := New()
		return ts.Load([]byte(`
types:
  article:
    admin: { display: ` + display + ` }
    fields:
      - { name: name, kind: text }
      - { name: state, kind: select, options: [draft, published] }
      - { name: cover, kind: upload-image }
`))
	}
	// 能当显示名的: 文本类标量（text / textarea / select / address / richtext）
	for _, good := range []string{"name", "state"} {
		if err := load(good); err != nil {
			t.Fatalf("display=%s 应当通过: %v", good, err)
		}
	}
	// 图片/引用/数字 这些当显示名就是配置错 —— 界面上会是一串路径或一个对象
	if err := load("cover"); err == nil || !strings.Contains(err.Error(), "不能当显示名") {
		t.Fatalf("display=cover 应当被拒: %v", err)
	}
	if err := load("nope"); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("display 指向没声明的字段应当被拒: %v", err)
	}
}
