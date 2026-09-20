// bigram 分词 —— CJK 连续段取 2 字滑窗, 其余（拉丁/数字）保留原词。
//
//	"人工智能与AI" → "人工 工智 智能 能与 AI"
//
// 为什么不用分词器（jieba 之类）:
//
//   - 中文**检索**里 bigram 的子串匹配召回更全（新词/专名自动覆盖, 不需要词典）
//   - 零依赖: 词典是几 MB 的资产 + 一个要跟着升级的东西; cgo 版（gojieba）还会
//     让站点失去 CGO_ENABLED=0 的单文件静态二进制
//
// 已知代价（bigram 方案的固有噪音）: "与" 也在 CJK 区 ⇒ 会和前一个字组成跨边界
// bigram（"能与"）。查询侧同样处理, 所以一致性没问题, 只是索引里多一点噪音。
//
// 单字词（"市"）: 只按 bigram 切会丢掉它（"深圳市" → "深圳 圳市"）—— 查询侧也按
// 同样规则切, 单字查询会切成单字词元, 两边一致即可命中。见 token_test.go。
package search

import "strings"

// bigram 把文本切成检索词元（空格连接）。
func bigram(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	runes := []rune(s)
	appendToken := func(token string) {
		if token == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(token)
	}

	start := 0
	inCJK := isCJK(runes[0])
	flush := func(end int) {
		segment := runes[start:end]
		if len(segment) == 0 {
			return
		}
		if !inCJK {
			// 拉丁/数字段: 原样保留（"AI"、"2026" 都是完整词元）。
			// 大小写不自己折叠 —— FTS5 的 unicode61 默认就折叠, 折两次是多余的一步。
			appendToken(strings.TrimSpace(string(segment)))
			return
		}
		if len(segment) == 1 {
			appendToken(string(segment)) // 单字: 自己成词元, 否则永远搜不到
			return
		}
		for i := 0; i < len(segment)-1; i++ {
			appendToken(string(segment[i : i+2]))
		}
	}
	for index, r := range runes {
		current := isCJK(r)
		if current != inCJK {
			flush(index)
			start = index
			inCJK = current
		}
	}
	flush(len(runes))
	return b.String()
}

// isCJK 常用汉字区（与 v2 同口径 —— 够用且零依赖; 扩展区/假名走非 CJK 分支,
// 原样保留成整词, 仍可命中）。
func isCJK(r rune) bool {
	return r >= 0x4e00 && r <= 0x9fff
}
