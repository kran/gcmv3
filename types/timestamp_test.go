package types

import "testing"

func TestTimestampKind(t *testing.T) {
	ts := New()
	if err := ts.Load([]byte(`
types:
  event:
    fields:
      - { name: name, kind: text }
      - { name: start_at, kind: timestamp }
`)); err != nil {
		t.Fatal(err)
	}
	td, _ := ts.Type("event")
	startAt := td.Fields[1]

	// 整数秒通过（int/int64 与 JSON 解出来的 float64 都收 —— JSON 里没有 int）。
	for _, good := range []any{
		int64(1789000000), 1700000000, float64(1789000000),
	} {
		if err := ts.ValidateValue("event", startAt, good); err != nil {
			t.Fatalf("%#v 应当通过（整数秒）: %v", good, err)
		}
	}
	// 其余一律拒: 字符串（旧表示）、带小数、毫秒/微秒/纳秒、非正数、垃圾。
	for _, bad := range []any{
		"2026-09-14T23:06:41Z", // v2 的 ISO 表示 —— 端口时最容易漏掉的那种
		"1789000000",
		"",
		1789000000.5,               // 不是整数秒
		int64(1789000000000),       // 毫秒（JS 的 Date.now()）
		int64(1789000000000000),    // 微秒
		int64(1789000000000000000), // 纳秒
		0, -1,                      // 0/负数不是时间点
		nil, true, []int{1}, // 其它类型
	} {
		if err := ts.ValidateValue("event", startAt, bad); err == nil {
			t.Fatalf("%#v 应当被拒（timestamp 只收整数秒）", bad)
		}
	}
	// 毫秒的报错要**点出毫秒** —— 这是唯一的高频单位错。
	err := ts.ValidateValue("event", startAt, int64(1789000000000))
	if err == nil || !contains(err.Error(), "毫秒") {
		t.Fatalf("毫秒的报错该说清单位: %v", err)
	}
	// required 检查用的空判断: 取不到整数或为 0 都算空。
	if !ts.isEmpty(KindTimestamp, nil) || !ts.isEmpty(KindTimestamp, 0) {
		t.Fatal("nil 与 0 应当算空")
	}
	if ts.isEmpty(KindTimestamp, 1789000000) {
		t.Fatal("正常值不该算空")
	}
	if k, _ := ts.Kind(KindTimestamp); k.Class() != ClassField {
		t.Fatalf("timestamp should be ClassField")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
