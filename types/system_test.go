package types

import (
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
