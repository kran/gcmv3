// 查询: FTS5 匹配 → 相关度序的 id 流 → 回读（掩码/读规则/展开自动生效）。
package search

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/kran/dba"
	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/web"
)

// Cursor 翻页游标: 相关度 + rowid（FTS5 的 rank 可以在 WHERE 里比较）。
//
// 带上 q/type: 换了关键词还用旧游标 ⇒ 直接报错（"翻到一半换了搜索词"是 bug,
// 不是"从头再来"——静默接受会给出看不懂的结果）。
type Cursor struct {
	Query string
	Type  string
	Rank  float64
	RowID int64
}

// encode 游标 → 不透明串（客户端只负责带回来）。
func (c Cursor) encode() string {
	raw := fmt.Sprintf("1\x00%s\x00%s\x00%s\x00%d", c.Query, c.Type, strconv.FormatFloat(c.Rank, 'g', 17, 64), c.RowID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor 解析游标（空串 = 第一页）。
func decodeCursor(raw string) (*Cursor, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("游标不合法")
	}
	parts := strings.Split(string(decoded), "\x00")
	if len(parts) != 5 || parts[0] != "1" {
		return nil, fmt.Errorf("游标不合法")
	}
	rank, err := strconv.ParseFloat(parts[3], 64)
	if err != nil {
		return nil, fmt.Errorf("游标不合法")
	}
	rowID, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("游标不合法")
	}
	return &Cursor{Query: parts[1], Type: parts[2], Rank: rank, RowID: rowID}, nil
}

// hit 一条命中（id + 类型 + 相关度）。
type hit struct {
	ID    int64
	Type  string
	Rank  float64
	RowID int64
}

// pair 命中 + 回读到的节点（相关度序）。
type pair struct {
	Hit  hit
	Node *core.Node
}

// searchHits 取一页命中 id（相关度序）。cursor 为空 = 第一页。
//
// want 是"想要的条数"（调用方会多要一点 —— 回读时有些节点对当前身份不可见）。
// more 表示 FTS 侧游标之后还有命中（可能都不可见 —— 那是读规则的事）。
func (p *Plugin) searchHits(db *dba.SQL, query, typeName string, cursor *Cursor, want int) ([]hit, bool, error) {
	bq := bigram(strings.TrimSpace(query))
	if bq == "" {
		return nil, false, fmt.Errorf("请输入搜索关键词")
	}
	match, found, err := p.resolveMatch(db, bq)
	if err != nil {
		return nil, false, err
	}
	if !found {
		// 一个词元都不在语料里 ⇒ 命中为空（这不是错误: 搜不到很正常）
		return nil, false, nil
	}

	// 类型过滤直接进 FTS 查询（type 是过滤列, bm25 权重 0）——
	// 不靠回读去筛: 否则每页的有效条数会被别的类型挤掉。
	sql := `SELECT rowid, type, rank FROM search_fts
		WHERE search_fts MATCH #{1}`
	args := []any{match}
	if typeName != "" {
		sql += ` AND type = #{2}`
		args = append(args, typeName)
	}
	if cursor != nil {
		// (rank, rowid) 游标: 严格大于, 同 rank 用 rowid 破平 —— 实测与 OFFSET 分页
		// 拼接结果完全一致（见 plugin 文档; rowid 唯一 ⇒ 不会重复也不会漏）
		sql += fmt.Sprintf(` AND (rank > #{%d} OR (rank = #{%d} AND rowid > #{%d}))`,
			len(args)+1, len(args)+1, len(args)+2)
		args = append(args, cursor.Rank, cursor.RowID)
	}
	sql += fmt.Sprintf(` ORDER BY rank, rowid LIMIT #{%d}`, len(args)+1)
	args = append(args, want+1) // 多取一条: 判断还有没有下一页

	rows, err := db.Add(sql, args...).FetchMaps()
	if err != nil {
		return nil, false, fmt.Errorf("检索失败: %w", err)
	}
	hits := make([]hit, 0, len(rows))
	for _, row := range rows {
		hits = append(hits, hit{
			ID:    toInt64(row["rowid"]),
			Type:  toString(row["type"]),
			Rank:  toFloat64(row["rank"]),
			RowID: toInt64(row["rowid"]),
		})
	}
	more := len(hits) > want
	if more {
		hits = hits[:want]
	}
	return hits, more, nil
}

// resolveMatch 三级放宽（与 v2 同口径）:
//
//  1. 整个查询当**一个短语**: 连续 bigram = 原文子串 ⇒ 精确、相关度最高
//  2. 丢掉语料里不存在的词元后 AND: "农业著名" → 农业 AND 著名
//  3. 还不行就 OR: 至少把沾边的捞回来（相关度排序会把最像的排前面）
//
// 返回 (match 表达式, 有没有命中, err)。found 为假 = 索引里确实没有 ⇒ 调用方直接
// 回空结果（不要编一个"保证不匹配"的表达式去骗 SQLite —— 那是自找语法麻烦）。
func (p *Plugin) resolveMatch(db *dba.SQL, bq string) (string, bool, error) {
	phrase := ftsPhrase(bq)
	found, err := p.matchExists(db, phrase)
	if err != nil {
		return "", false, err
	}
	if found {
		return phrase, true, nil
	}
	present, err := p.presentTokens(db, bq)
	if err != nil {
		return "", false, err
	}
	if len(present) == 0 {
		return "", false, nil
	}
	if len(present) == 1 {
		return present[0], true, nil
	}
	and := strings.Join(present, " AND ")
	found, err = p.matchExists(db, and)
	if err != nil {
		return "", false, err
	}
	if found {
		return and, true, nil
	}
	return strings.Join(present, " OR "), true, nil
}

// matchExists 这个 MATCH 表达式在索引里有没有命中（词元探针 —— 判断的是
// "这个词存在不存在", 与当前身份的读范围无关）。
func (p *Plugin) matchExists(db *dba.SQL, match string) (bool, error) {
	row, err := db.Add(`SELECT 1 FROM search_fts WHERE search_fts MATCH #{1} LIMIT 1`, match).FetchOne[int]()
	if err != nil {
		return false, fmt.Errorf("检索失败: %w", err)
	}
	return row != nil, nil
}

// presentTokens 只保留语料里真实存在的词元（接缝噪音 bigram 在这里被丢掉）。
func (p *Plugin) presentTokens(db *dba.SQL, bq string) ([]string, error) {
	kept := []string{}
	for _, token := range strings.Fields(bq) {
		phrase := ftsPhrase(token)
		found, err := p.matchExists(db, phrase)
		if err != nil {
			return nil, err
		}
		if found {
			kept = append(kept, phrase)
		}
	}
	return kept, nil
}

// ftsPhrase 把一个词元写成 FTS5 短语字面量（内部 " 翻倍转义）。
//
// MATCH 后面的字符串会被 FTS5 再解析一次（参数绑定只保护 SQL 那一层）⇒ 每个词元
// 都包成短语, 关键字和操作符就只是普通词: AND/OR/NOT/NEAR 不再是操作符,
// a:b 不再是列过滤, abc* 不再是前缀查询, 单个 " 也不再是语法错误。
func ftsPhrase(token string) string {
	return `"` + strings.ReplaceAll(token, `"`, `""`) + `"`
}

// loadVisible 回读这一页命中: 走 ctx.List ⇒ 读规则（行范围）、字段掩码、展开
// 全部自动生效。不可见的命中在这里被丢掉（顺序仍按相关度）。
//
// 跨类型时按类型分组回读（ctx.List 是按类型的读入口）—— 回读后按原顺序重排。
func (p *Plugin) loadVisible(ctx *web.CmsCtx, hits []hit) ([]*core.Node, error) {
	byType := map[string][]int64{}
	for _, h := range hits {
		byType[h.Type] = append(byType[h.Type], h.ID)
	}
	loaded := map[int64]*core.Node{}
	for typeName, ids := range byType {
		nodes, _, err := ctx.List(core.NodeQuery{
			// 路径 token 的写法: "$字段" 是普通字段, "id" 是系统列（不带 $）
			Type: typeName, Where: so.P("in", "id", ids),
		}, len(ids), 0)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			loaded[node.ID] = node
		}
	}
	out := make([]*core.Node, 0, len(hits))
	for _, h := range hits {
		node, ok := loaded[h.ID]
		if !ok {
			continue // 不可见（读规则裁掉了）—— 不是错误
		}
		out = append(out, node)
	}
	return out, nil
}

func toInt64(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	}
	return 0
}

func toFloat64(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int64:
		return float64(typed)
	}
	return 0
}

func toString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	}
	return ""
}
