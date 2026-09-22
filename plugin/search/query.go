// 查询: FTS5 匹配 → 相关度序的 id 流 → 回读（掩码/读规则/展开自动生效）。
package search

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kran/dba"
	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/web"
)

// Cursor 翻页游标: **偏移**（不是 (rank, rowid)）。
//
// 为什么变了: 现在每一次查询都要把「短语列表 + OR 列表」重新融合一遍, 融合后的序是
// **确定**的（同样的输入 ⇒ 同样的序）⇒ 用"窗口大小 + 偏移"就能稳定翻页。bm25 的
// (rank,rowid) 在融合之后不再描述顺序了。
//
// 带上 q/type: 换了关键词还用旧游标 ⇒ 直接报错（"翻到一半换了搜索词"是 bug,
// 不是"从头再来"——静默接受会给出看不懂的结果）。
type Cursor struct {
	Query  string
	Type   string
	Offset int
}

// encode 游标 → 不透明串（客户端只负责带回来）。
func (c Cursor) encode() string {
	raw := fmt.Sprintf("2\x00%s\x00%s\x00%d", c.Query, c.Type, c.Offset)
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
	// 版本 2 = 偏移游标（版本 1 是 (rank,rowid) —— 语义变了就不认旧的, 宁可报错）
	if len(parts) != 4 || parts[0] != "2" {
		return nil, fmt.Errorf("游标不合法")
	}
	offset, err := strconv.Atoi(parts[3])
	if err != nil || offset < 0 {
		return nil, fmt.Errorf("游标不合法")
	}
	return &Cursor{Query: parts[1], Type: parts[2], Offset: offset}, nil
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
	Hit    hit
	Node   *core.Node
	Phrase bool // 命中里包含用户输入的**连续短语**（前端据此提示"完全匹配"）
}

// ftsTop 取相关度最高的 want 条命中（不要游标: 融合需要的是"从头开始的排名"）。
//
// 类型过滤直接进 FTS 查询（type 是过滤列, bm25 权重 0）—— 不靠回读去筛:
// 否则每页的有效条数会被别的类型挤掉。
func (p *Plugin) ftsTop(db *dba.SQL, match, typeName string, want int) ([]hit, bool, error) {
	sql := `SELECT rowid, type, rank FROM search_fts WHERE search_fts MATCH #{1}`
	args := []any{match}
	if typeName != "" {
		sql += ` AND type = #{2}`
		args = append(args, typeName)
	}
	sql += fmt.Sprintf(` ORDER BY rank, rowid LIMIT #{%d}`, len(args)+1)
	args = append(args, want+1) // 多取一条: 判断还有没有更多

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

// rrfK RRF 的常数（Cormack 等 2009 的 60）: 把"排名差"压得很小, 靠多路叠加取胜。
// 好处是对两路的**量纲不敏感** —— 不必把 bm25 归一化到 0~1（那是调参无底洞）。
const rrfK = 60

// rankedHit 融合后的一条: 命中 + 分数 + 来自哪几路。
type rankedHit struct {
	hit
	Phrase bool
	Score  float64
}

// searchRanked 一次查询的**排序结果**: 不再"选一级"（三级放宽）, 而是
//
//	① 语料里存在的词元全部 OR 起来 → FTS5 按 bm25 排序（这就是"匹配度"）
//	② 整句作为**短语**再跑一路（连续 bigram = 原文子串 ⇒ 命中即强信号）
//	③ 两路用 RRF 融合（1/(k+排名) 相加）⇒ 短语命中的提前
//
// 为什么这是对的: 搜索不是"筛选", 是"打分"。给了十个词, 命中八成的该排在命中两成的
// 前面, 而不是"非得全中才算命中"。语料里不存在的词元直接丢（用户多打一个字不该把
// 结果清零）—— 这一条是从原来的第二级里保留下来的, 但它不再是"一级", 只是构造 OR 时
// 的清理。
func (p *Plugin) searchRanked(db *dba.SQL, query, typeName string, window int) ([]rankedHit, bool, error) {
	bq := bigram(strings.TrimSpace(query))
	if bq == "" {
		return nil, false, fmt.Errorf("请输入搜索关键词")
	}
	present, err := p.presentTokens(db, bq)
	if err != nil {
		return nil, false, err
	}
	if len(present) == 0 {
		// 一个词元都不在语料里 ⇒ 命中为空（搜不到很正常, 不是错误）
		return nil, false, nil
	}

	orHits, orMore, err := p.ftsTop(db, strings.Join(present, " OR "), typeName, window)
	if err != nil {
		return nil, false, err
	}
	phraseHits, phraseMore, err := p.ftsTop(db, ftsPhrase(bq), typeName, window)
	if err != nil {
		return nil, false, err
	}

	merged := map[int64]*rankedHit{}
	order := make([]int64, 0, len(orHits)+len(phraseHits))
	add := func(hits []hit, isPhrase bool) {
		for index, one := range hits {
			entry, ok := merged[one.ID]
			if !ok {
				entry = &rankedHit{hit: one}
				merged[one.ID] = entry
				order = append(order, one.ID)
			}
			entry.Score += 1 / float64(rrfK+index+1)
			if isPhrase {
				entry.Phrase = true
				// 短语那一等第已经在上面加过了（两路权重相等 —— 教科书 RRF）。
				// 想让短语更强势就再给一路加成, 这里刻意留白: 先看真实效果再调。
			}
		}
	}
	add(orHits, false)
	add(phraseHits, true)

	out := make([]rankedHit, 0, len(order))
	for _, id := range order {
		out = append(out, *merged[id])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].RowID < out[j].RowID // 破平: rowid 保证稳定（可重算的序）
	})
	return out, orMore || phraseMore, nil
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
