package types

import (
	"fmt"
	"math"
)

// timestampKind 时间点。值 = **Unix 秒（int64, UTC 绝对时刻）**。
//
// 为什么是整数秒而不是 ISO 字符串（v2 用的是字符串, gcmv3 改过来）:
//
//   - **一个系统一种时间**: 行/列（created_at/updated_at/expires_at）、字段值、会话过期
//     全是一种表示。v2 是"列用整数、字段用字符串"——于是前端控件里得挂两条补丁
//     （"旧格式警告" + "别把数字当毫秒"), 那些补丁的存在本身就说明表示不统一。
//   - **比较/排序是数值比较**: 不再依赖"必须归一化到定宽字符串"这条隐式契约
//     （那条契约一破（带偏移、带毫秒）字典序就不再是时间序, 而破法静默）。
//   - **没有格式/单位歧义**: 客户端传的是绝对时刻。唯一的坑是 JS 的 `Date.now()` 是毫秒,
//     所以这里**当场拒绝毫秒级的值**（见 maxTimestamp）。
//
// 库里要看人话: `SELECT datetime(published_at,'unixepoch') FROM …` 或
// `json_extract(fields,'$.published_at')` 自己换算。
type timestampKind struct{}

// KindTimestamp kind 名（值 = Unix 秒）。
const KindTimestamp = "timestamp"

// maxTimestamp 秒的合理上限（约公元 5138 年）。毫秒/微秒/纳秒级的时间戳都超过它 ——
// 单位写错当场炸, 不留"1970 年"这种静默损坏。
const maxTimestamp = 100_000_000_000

func (timestampKind) Name() string { return KindTimestamp }

func (timestampKind) Validate(_ FieldDef, v any) error {
	seconds, ok := UnixSeconds(v)
	if !ok {
		return fmt.Errorf("expects unix seconds (integer), got %T(%v)", v, v)
	}
	if seconds <= 0 {
		return fmt.Errorf("expects a positive unix timestamp, got %d", seconds)
	}
	if seconds > maxTimestamp {
		return fmt.Errorf("expects unix **seconds**, got %d —— 这看起来是毫秒（JS 的 Date.now() 要除 1000）", seconds)
	}
	return nil
}

// IsEmpty 空/非法（required 检查用）: 取不到整数或为 0 都算空（0 = 1970, 当未设置）。
func (timestampKind) IsEmpty(v any) bool {
	seconds, ok := UnixSeconds(v)
	return !ok || seconds == 0
}

func (timestampKind) ValidateField(t *Types, typeName string, f FieldDef, defs map[string]TypeDef) error {
	return rejectRefAttrs(typeName, f)
}
func (timestampKind) Class() Class { return ClassField }
func (timestampKind) QueryOps() QueryOps {
	return QueryOps{Equal: true, Ordered: true, Sortable: true}
}

// UnixSeconds 把时间字段的值取成 Unix 秒 —— 类型里的整数（int/int64）与 JSON 解出来的
// 浮点（float64, 必须是整数值）都收; 别的形态一律不认（fail-loud, 不猜单位）。
//
// 站点代码要读时间字段也用它（比裸断言稳）: `types.UnixSeconds(fields["start_at"])`。
func UnixSeconds(v any) (int64, bool) {
	switch typed := v.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case float64:
		if typed != math.Trunc(typed) {
			return 0, false
		}
		return int64(typed), true
	}
	return 0, false
}
