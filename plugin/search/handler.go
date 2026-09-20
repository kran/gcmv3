// 检索端点（插件自己挂: Options.Prefix, 默认 /api/search）。
package search

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/web"
)

// roundLimit 一次请求最多取几轮命中窗口。
//
// 回读会丢掉"对当前身份不可见"的命中 ⇒ 命中集里混着大量不可见节点时, 一窗可能
// 凑不满一页。多取几轮（每轮都能填满的话第一轮就走了）; 最多 3 轮, 免得把
// "整页都不可见"变成一次无穷扫描。
const roundLimit = 3

// handle GET /api/search?q=&type=&size=&cursor=
//
// 响应: {items, has_more, next_cursor, size, type, q}
//
//	items       已经过读规则/掩码/展开的节点（相关度序）
//	has_more    FTS 侧还有命中（可能都不可见 —— 所以还要看 items 是否为空）
//	next_cursor 下一页游标（has_more 为假时没有）
//
// 没有 total: 精确计数要扫完整个命中集（实测 10 万命中约 7s）, 而它唯一的用途是
// "判断还有没有下一页" —— 那是 has_more 的活。
func (p *Plugin) handle(ctx *web.CmsCtx) {
	query := strings.TrimSpace(ctx.Query("q"))
	if query == "" {
		ctx.Fail(web.BadRequest("请输入搜索关键词"))
		return
	}
	if utf8.RuneCountInString(query) > p.maxRunes {
		ctx.Fail(web.BadRequest("搜索关键词过长（最多 %d 个字符）", p.maxRunes))
		return
	}
	typeName := strings.TrimSpace(ctx.Query("type"))
	if typeName != "" && !p.searchable(typeName) {
		ctx.Fail(web.BadRequest("不支持的搜索类型: %s", typeName))
		return
	}
	size, err := p.pageSize(ctx.Query("size"))
	if err != nil {
		ctx.Fail(err)
		return
	}
	cursor, err := decodeCursor(ctx.Query("cursor"))
	if err != nil {
		ctx.Fail(web.BadRequest("%s", err.Error()))
		return
	}
	if cursor != nil && (cursor.Query != query || cursor.Type != typeName) {
		ctx.Fail(web.BadRequest("游标与本次检索不匹配（搜索词或类型变了）—— 从第一页重新开始"))
		return
	}

	pairs, hasMore, err := p.collect(ctx, query, typeName, cursor, size)
	if err != nil {
		ctx.Fail(err)
		return
	}
	items := make([]*core.Node, 0, len(pairs))
	for _, item := range pairs {
		items = append(items, item.Node)
	}

	payload := map[string]any{
		"items": items, "has_more": hasMore, "size": size, "q": query, "type": typeName,
	}
	if hasMore && len(pairs) > 0 {
		// 游标 = **最后一条返回的**命中（不是最后扫描的）: 窗口里没返回的那些下一轮
		// 会重新扫到, 但不会漏（不可见的会被再丢一次, 代价有界）。
		last := pairs[len(pairs)-1].Hit
		next := Cursor{Query: query, Type: typeName, Rank: last.Rank, RowID: last.RowID}
		payload["next_cursor"] = next.encode()
	}
	_ = ctx.Json(http.StatusOK, payload)
}

// collect 取到"够一页可见结果"为止（最多 roundLimit 轮）。
//
// 返回**至多 size 条**可见结果（相关度序）+ "FTS 侧还有没有更多"。
// 不可见的命中会被消费掉（游标越过它）—— 否则它们一直占着窗口, 每页都凑不满。
func (p *Plugin) collect(ctx *web.CmsCtx, query, typeName string, cursor *Cursor, size int) ([]pair, bool, error) {
	db := ctx.DB()
	want := size*3 + 10 // 一窗多取一点: 不可见的命中不必再来一轮
	out := make([]pair, 0, size)
	for round := 0; round < roundLimit; round++ {
		hits, more, err := p.searchHits(db, query, typeName, cursor, want)
		if err != nil {
			return nil, false, err
		}
		if len(hits) == 0 {
			return out, false, nil
		}
		nodes, err := p.loadVisible(ctx, hits)
		if err != nil {
			return nil, false, err
		}
		visible := make(map[int64]*core.Node, len(nodes))
		for _, node := range nodes {
			visible[node.ID] = node
		}
		lastReturned := -1
		for index, h := range hits {
			cursor = &Cursor{Query: query, Type: typeName, Rank: h.Rank, RowID: h.RowID}
			node, ok := visible[h.ID]
			if !ok {
				continue // 不可见: 消费掉, 不返回
			}
			if len(out) < size {
				out = append(out, pair{Hit: h, Node: node})
				lastReturned = index
			}
		}
		if len(out) >= size {
			// 够一页了: 游标退回到**最后一条返回的**（窗口里排在它后面的可见结果
			// 下一轮还会扫到 —— 不能越过, 否则漏数据）
			if lastReturned >= 0 {
				last := hits[lastReturned]
				cursor = &Cursor{Query: query, Type: typeName, Rank: last.Rank, RowID: last.RowID}
			}
			hasMore := true
			return out, hasMore, nil
		}
		if !more {
			return out, false, nil
		}
	}
	return out, true, nil
}

// pageSize 解析 size（默认 DefaultSize, 上限 MaxSize —— 上限是给"客户端参数"用的,
// 不是给站点内部读用的）。
func (p *Plugin) pageSize(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultSize, nil
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size <= 0 {
		return 0, web.BadRequest("size 必须是正整数")
	}
	if size > MaxSize {
		size = MaxSize
	}
	return size, nil
}
