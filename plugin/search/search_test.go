package search

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/web"
)

// 测试夹具: 一个真站点（真 sqlite 文件 + 真 HTTP）+ 装好的 search 插件。
const testTypes = `
types:
  article:
    capabilities: { addressable: true }
    fields:
      - { name: name, kind: text }
      - { name: body, kind: richtext }
      - { name: state, kind: select, options: [draft, published], default: draft }
      - { name: phone, kind: text }
  event:
    fields:
      - { name: name, kind: text }
      - { name: body, kind: richtext }
  # 认证渠道要挂在一个真实存在的类型上（realm.node_type 拼错 ⇒ 注册当场 panic）
  member:
    capabilities:
      authentication: { roles: [秘书处] }
    fields:
      - { name: name, kind: text }
`

type harness struct {
	site    *web.Site
	handler http.Handler
	plugin  *Plugin
	token   string
}

func newHarness(t *testing.T, options Options, configure ...func(*web.Site)) *harness {
	t.Helper()
	basedir := t.TempDir()
	err := os.WriteFile(filepath.Join(basedir, "site.yaml"), []byte(testTypes), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	site, err := web.Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = site.Close() })

	// 读规则: 全放行（可见性/掩码由用例通过 configure 覆盖 —— 策略必须在 Setup 之前
	// 注册, 框架会拦"Setup 之后改策略"）。
	for _, typeName := range []string{"article", "event"} {
		site.Type(typeName).OnRead(func(_ *web.CmsCtx, where *so.Where, _ *web.Grant) error {
			*where = so.P("true")
			return nil
		})
	}
	for _, fn := range configure {
		fn(site)
	}
	// 认证渠道: 测试用 CreateSession(realm="frontend") 造身份
	site.Auth().Register(web.AuthRealm{Name: "frontend", NodeType: "member", Default: true})

	if options.Types == nil {
		options.Types = []string{"article", "event"}
	}
	plugin, err := Mount(site, options)
	if err != nil {
		t.Fatal(err)
	}

	memberID, err := site.Engine().RegisterAuth(nil, "member", "email", "reader@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "读者"}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "frontend", memberID)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{site: site, handler: site.Setup(), plugin: plugin, token: token}
}

// create 直接用引擎建节点（写事件照常触发 ⇒ 索引同步）。
func (h *harness) create(t *testing.T, typeName string, fields core.Fields) int64 {
	t.Helper()
	id, err := h.site.Engine().CreateNode(nil, &core.Node{Type: typeName, Fields: fields})
	if err != nil {
		t.Fatalf("建 %s: %v", typeName, err)
	}
	return id
}

// search 调检索端点。
func (h *harness) search(t *testing.T, params map[string]string) map[string]any {
	t.Helper()
	query := ""
	for key, value := range params {
		if query != "" {
			query += "&"
		}
		// 必须转义: "新能源 产业 会员" 这种多词查询带空格, 直接拼会把请求行拼坏
		query += key + "=" + url.QueryEscape(value)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/search?"+query, nil)
	request.Header.Set("Authorization", "Bearer "+h.token)
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("检索 %v = %d: %s", params, response.Code, response.Body.String())
	}
	out := map[string]any{}
	err := json.Unmarshal(response.Body.Bytes(), &out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// titles 从响应里取节点名（列表项是完整节点, 名字在 extra.display）。
func titles(t *testing.T, payload map[string]any) []string {
	t.Helper()
	items, _ := payload["items"].([]any)
	out := []string{}
	for _, raw := range items {
		node, _ := raw.(map[string]any)
		extra, _ := node["extra"].(map[string]any)
		name, _ := extra["display"].(string)
		out = append(out, name)
	}
	return out
}

// 写节点就进索引（写事件在核心, 与站点规则无关）。
func TestIndexFollowsWrites(t *testing.T) {
	h := newHarness(t, Options{})
	h.create(t, "article", core.Fields{"name": "新能源产业对接会", "body": "<p>深圳举办</p>"})

	payload := h.search(t, map[string]string{"q": "新能源"})
	if got := titles(t, payload); len(got) != 1 || got[0] != "新能源产业对接会" {
		t.Fatalf("搜\"新能源\"应命中 1 条, 实际 %#v", got)
	}
	// 正文也能搜到（richtext 的 QueryOps.Text 为真 ⇒ 自动进索引, 不必单独声明）
	payload = h.search(t, map[string]string{"q": "深圳"})
	if got := titles(t, payload); len(got) != 1 {
		t.Fatalf("正文该可检索, 实际 %#v", got)
	}
}

// 子串（不必整词）+ 短语精确: bigram 方案的核心能力。
func TestSearchSubstring(t *testing.T) {
	h := newHarness(t, Options{})
	h.create(t, "article", core.Fields{"name": "2026年新能源产业对接会在深圳举办"})

	for _, query := range []string{"新能源", "产业对接", "对接会", "在深圳", "2026"} {
		payload := h.search(t, map[string]string{"q": query})
		if got := titles(t, payload); len(got) != 1 {
			t.Fatalf("搜 %q 应命中（子串级召回）, 实际 %#v", query, got)
		}
	}
}

// 更新与删除同步索引。
func TestIndexFollowsUpdateAndDelete(t *testing.T) {
	h := newHarness(t, Options{})
	id := h.create(t, "article", core.Fields{"name": "旧牌子在这里"})

	if got := titles(t, h.search(t, map[string]string{"q": "旧牌子"})); len(got) != 1 {
		t.Fatalf("改之前该搜得到: %#v", got)
	}
	err := h.site.Engine().PatchNode(nil, id, &core.NodePatch{
		Revision: ptr(int64(1)), Fields: core.Fields{"name": "新招牌在这里"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(t, h.search(t, map[string]string{"q": "新招牌"})); len(got) != 1 {
		t.Fatalf("改之后该按新值搜到: %#v", got)
	}
	// 注意: 这里用"没有任何共享 bigram"的词 —— 有共享 bigram 时放宽兜底（三级里的
	// AND/OR）会照旧命中, 那是召回设计, 不是索引没更新。
	if got := titles(t, h.search(t, map[string]string{"q": "旧牌子"})); len(got) != 0 {
		t.Fatalf("旧值不该还在索引里: %#v", got)
	}
	if err := h.site.Engine().DeleteNode(nil, id); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, h.search(t, map[string]string{"q": "新招牌"})); len(got) != 0 {
		t.Fatalf("删了之后不该还搜得到: %#v", got)
	}
}

// **策略不会被绕过**: 节点对当前身份不可见 ⇒ 检索结果里也没有它。
func TestSearchRespectsReadScope(t *testing.T) {
	// 读规则: 只有已发布可见（在 Setup 之前注册）
	h := newHarness(t, Options{}, func(site *web.Site) {
		site.Type("article").OnRead(func(_ *web.CmsCtx, where *so.Where, _ *web.Grant) error {
			*where = so.P("=", "$state", "published")
			return nil
		})
	})
	h.create(t, "article", core.Fields{"name": "草稿里的秘密", "state": "draft"})
	h.create(t, "article", core.Fields{"name": "已发布的公告", "state": "published"})

	got := titles(t, h.search(t, map[string]string{"q": "秘密"}))
	if len(got) != 0 {
		t.Fatalf("不可见的节点不该出现在检索结果里: %#v", got)
	}
	if got := titles(t, h.search(t, map[string]string{"q": "公告"})); len(got) != 1 {
		t.Fatalf("可见的该照常命中: %#v", got)
	}
}

// 掩码照旧: 命中节点里被裁掉的字段不出现在响应里（检索是另一个入口, 不是旁路）。
func TestSearchRespectsMasking(t *testing.T) {
	h := newHarness(t, Options{}, func(site *web.Site) {
		site.Type("article").OnRead(func(_ *web.CmsCtx, where *so.Where, masked *web.Grant) error {
			*where = so.P("true")
			masked.Add("public", "phone")
			return nil
		})
	})
	h.create(t, "article", core.Fields{"name": "会员名录", "phone": "13800138000"})

	payload := h.search(t, map[string]string{"q": "名录"})
	items, _ := payload["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("该命中 1 条: %#v", payload)
	}
	node, _ := items[0].(map[string]any)
	fields, _ := node["fields"].(map[string]any)
	if _, ok := fields["phone"]; ok {
		t.Fatalf("被掩码的字段不该出现在检索响应里: %#v", fields)
	}
	// 列表形状的响应不带 masked/editable 事实（框架的既定口径: 逐项跑规则太贵,
	// 只有单节点详情算）—— 检索结果是"列表", 所以这里只保证值被裁掉了。
	if maskedNames, ok := node["masked"].([]any); ok && len(maskedNames) != 0 {
		t.Fatalf("列表形状不该带事实: %#v", maskedNames)
	}
}

// 类型过滤 + 游标翻页: 两页拼起来 == 一次取更多, 且不重不漏。
func TestSearchCursorPaging(t *testing.T) {
	h := newHarness(t, Options{})
	for i := 0; i < 7; i++ {
		h.create(t, "article", core.Fields{"name": fmt.Sprintf("新能源会议第%d号", i), "state": "published"})
	}
	h.create(t, "event", core.Fields{"name": "新能源论坛"})

	// 全部类型: 一页一页翻到底（游标翻完必须与"一次直取"完全一致 —— 不重不漏不漂）
	all := []string{}
	params := map[string]string{"q": "新能源", "size": "3"}
	pages := 0
	for {
		page := h.search(t, params)
		all = append(all, titles(t, page)...)
		pages++
		if pages > 10 {
			t.Fatal("翻页没有终止（has_more 一直是 true？）")
		}
		more, _ := page["has_more"].(bool)
		if !more {
			break
		}
		cursor, _ := page["next_cursor"].(string)
		if cursor == "" {
			t.Fatalf("has_more 为真却没给 next_cursor: %#v", page)
		}
		params = map[string]string{"q": "新能源", "size": "3", "cursor": cursor}
	}

	direct := titles(t, h.search(t, map[string]string{"q": "新能源", "size": "10"}))
	if len(all) != len(direct) {
		t.Fatalf("游标翻完 %d 条 vs 直取 %d 条（%#v vs %#v）", len(all), len(direct), all, direct)
	}
	for i := range all {
		if all[i] != direct[i] {
			t.Fatalf("游标翻页与直取顺序不一致: %#v vs %#v", all, direct)
		}
	}

	// 类型过滤: 只剩 event
	events := titles(t, h.search(t, map[string]string{"q": "新能源", "type": "event"}))
	if len(events) != 1 || events[0] != "新能源论坛" {
		t.Fatalf("类型过滤该只剩 event: %#v", events)
	}
}

// fail-loud: 配错就当场报错, 不静默"永远搜不到"。
func TestMountFailsLoud(t *testing.T) {
	basedir := t.TempDir()
	err := os.WriteFile(filepath.Join(basedir, "site.yaml"), []byte(testTypes), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	site, err := web.Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = site.Close() }()

	if _, err := Mount(site, Options{}); err == nil {
		t.Fatal("空 Types 该报错")
	}
	if _, err := Mount(site, Options{Types: []string{"nope"}}); err == nil {
		t.Fatal("类型名写错该报错")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("错误信息要点名类型: %v", err)
	}
}

// 端点参数校验: 空 q / 坏游标 / 不支持的类型 / 超长关键词。
func TestHandlerValidation(t *testing.T) {
	h := newHarness(t, Options{})
	cases := []struct {
		path string
		want int
	}{
		{"/api/search?q=", http.StatusBadRequest},
		{"/api/search?q=%E6%96%B0&type=nope", http.StatusBadRequest},
		{"/api/search?q=x&cursor=not-a-cursor", http.StatusBadRequest},
		{"/api/search?q=" + strings.Repeat("%E6%96%B0", 101), http.StatusBadRequest},
	}
	for _, c := range cases {
		request := httptest.NewRequest(http.MethodGet, c.path, nil)
		request.Header.Set("Authorization", "Bearer "+h.token)
		response := httptest.NewRecorder()
		h.handler.ServeHTTP(response, request)
		if response.Code != c.want {
			t.Fatalf("%s = %d, 期望 %d: %s", c.path, response.Code, c.want, response.Body.String())
		}
	}
}

// Rebuild 全量重建（先清空再索引, 幂等）。
func TestRebuild(t *testing.T) {
	h := newHarness(t, Options{})
	h.create(t, "article", core.Fields{"name": "重建目标", "state": "published"})

	_, err := h.site.DB().Add(`DELETE FROM search_fts`).Exec()
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(t, h.search(t, map[string]string{"q": "重建"})); len(got) != 0 {
		t.Fatalf("清空后该搜不到: %#v", got)
	}
	if err := h.plugin.Rebuild(); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, h.search(t, map[string]string{"q": "重建"})); len(got) != 1 {
		t.Fatalf("重建后该搜得到: %#v", got)
	}
}

func ptr(v int64) *int64 { return &v }

// 给一大串词: **命中越多的排越前** —— 这是"搜索引擎式"的核心（不再要求全部命中）。
//
// 老行为是"三级放宽": 先整句短语、再 AND、再 OR —— 每次只**选一级**当过滤条件,
// 于是"命中 8 成"和"命中 2 成"在结果里没有区别（甚至 AND 级直接把少命中的全滤掉）。
func TestRankedByCoverage(t *testing.T) {
	h := newHarness(t, Options{})
	h.create(t, "article", core.Fields{"name": "新能源产业对接会", "body": "新能源 产业 对接 会员 企业"})
	h.create(t, "article", core.Fields{"name": "新能源政策", "body": "新能源 补贴 政策"})
	h.create(t, "article", core.Fields{"name": "会员走访", "body": "会员 企业 走访"})

	payload := h.search(t, map[string]string{"q": "新能源 产业 会员"})
	got := titles(t, payload)
	// 三篇都该出现（OR 是候选集），但顺序按命中度: 全中的那篇第一
	if len(got) != 3 {
		t.Fatalf("三篇都该被捞回来（OR 候选集）, 实际 %#v", got)
	}
	if got[0] != "新能源产业对接会" {
		t.Fatalf("命中三个词的那篇该排第一, 实际 %#v", got)
	}
	if matched, _ := payload["matched"].(string); matched != "any" {
		t.Fatalf("没有连续短语命中时 matched 该是 any, 实际 %q", matched)
	}
}

// 语料里不存在的词元不该把结果清零（老第二级的能力, 保留）。
func TestUnknownTokenDoesNotEmptyResults(t *testing.T) {
	h := newHarness(t, Options{})
	h.create(t, "article", core.Fields{"name": "商会动态", "body": "商会 新闻"})
	payload := h.search(t, map[string]string{"q": "商会 这个词肯定不存在zz"})
	if got := titles(t, payload); len(got) != 1 || got[0] != "商会动态" {
		t.Fatalf("多余的不存在词元不该清空结果, 实际 %#v", got)
	}
}

// 整句连续命中（短语）该排在最前, 并让响应标出 matched=phrase。
func TestPhraseRanksFirstAndFlagsMatched(t *testing.T) {
	h := newHarness(t, Options{})
	h.create(t, "article", core.Fields{"name": "商会章程", "body": "本会章程规定会员权利与义务"})
	h.create(t, "article", core.Fields{"name": "会员活动通知", "body": "商会 会员 活动 通知 公告"})

	payload := h.search(t, map[string]string{"q": "商会章程"})
	got := titles(t, payload)
	if len(got) == 0 || got[0] != "商会章程" {
		t.Fatalf("含连续短语的那篇该排第一, 实际 %#v", got)
	}
	if matched, _ := payload["matched"].(string); matched != "phrase" {
		t.Fatalf("有短语命中时 matched 该是 phrase, 实际 %q", matched)
	}
}
