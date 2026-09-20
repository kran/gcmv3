package search

import "testing"

func TestBigram(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		// 纯 CJK: 2 字滑窗
		{"新能源", "新能 能源"},
		{"深圳市恒新", "深圳 圳市 市恒 恒新"},
		// 单字: 自己成词元（否则"市"永远搜不到）
		{"市", "市"},
		// CJK 与非 CJK 交替: 拉丁段原样、小写化
		{"人工智能与AI", "人工 工智 智能 能与 AI"},
		{"AI与人工智能", "AI 与人 人工 工智 智能"},
		{"2026新能源产业对接会", "2026 新能 能源 源产 产业 业对 对接 接会"},
		// 标点是"非 CJK" ⇒ 自成词元（与 v2 同口径, 简单可预期, 不去标点）
		{"新能源,产业", "新能 能源 , 产业"},
		{"  hello world  ", "hello world"},
	}
	for _, c := range cases {
		if got := bigram(c.in); got != c.want {
			t.Errorf("bigram(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// 关键性质: 查询侧与索引侧同一套切法 ⇒ 子串查询必然命中。
// 这里只验"查询词元是原文词元的**连续子序列**"（FTS5 的 phrase 查询就靠它）。
func TestBigramPhraseIsSubstring(t *testing.T) {
	doc := bigram("2026年新能源产业对接会在深圳举办")
	query := bigram("新能源产业")
	if doc == "" || query == "" {
		t.Fatal("不该为空")
	}
	// 查询词元序列必须原样出现在文档词元序列里（连续）
	docTokens := splitTokens(doc)
	queryTokens := splitTokens(query)
	found := false
	for i := 0; i+len(queryTokens) <= len(docTokens); i++ {
		same := true
		for j, token := range queryTokens {
			if docTokens[i+j] != token {
				same = false
				break
			}
		}
		if same {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("查询词元 %q 不是文档词元 %q 的连续子序列（phrase 查询会失效）", query, doc)
	}
}

func splitTokens(s string) []string {
	out := []string{}
	current := ""
	for _, r := range s {
		if r == ' ' {
			if current != "" {
				out = append(out, current)
			}
			current = ""
			continue
		}
		current += string(r)
	}
	if current != "" {
		out = append(out, current)
	}
	return out
}
