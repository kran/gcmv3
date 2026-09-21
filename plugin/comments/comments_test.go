package comments

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
	"github.com/kran/gcmv3/web"
)

// 测试夹具: 一个真站点（真 sqlite 文件 + 真 HTTP）+ 装好的 comments 插件。
//
// 评论的可见性/归属是**站点策略**的事 ⇒ 夹具里也照站点的写法注册一份（公众只看
// approved、作者看得到自己那条待审、秘书处全看）, 不然测的就不是真场景。
const testTypes = `
types:
  article:
    capabilities: { addressable: true }
    fields:
      - { name: name, kind: text }
      - { name: body, kind: richtext }
  event:
    fields:
      - { name: name, kind: text }
  comment:
    admin: { label: 评论, display: body }
    fields:
      - { name: body, kind: textarea, required: true }
      - { name: target, kind: ref, to: article, required: true }
      - { name: parent, kind: ref, to: comment }
      - { name: author, kind: ref, to: member }
      - { name: state, kind: select, options: [pending, approved, spam], default: pending }
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
	member  int64
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

	for _, typeName := range []string{"article", "event"} {
		site.Type(typeName).OnRead(func(_ *web.CmsCtx, where *so.Where, _ *web.Grant) error {
			*where = so.P("true")
			return nil
		})
	}
	registerCommentPolicy(site)
	for _, fn := range configure {
		fn(site)
	}
	site.Auth().Register(web.AuthRealm{Name: "frontend", NodeType: "member", Default: true})

	if options.Types == nil {
		options.Types = []string{"article"}
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
	return &harness{site: site, handler: site.Setup(), plugin: plugin, token: token, member: memberID}
}

// registerCommentPolicy 站在站点一侧注册评论的读写规则（与 association/hooks.go 同形）。
func registerCommentPolicy(site *web.Site) {
	site.Type("comment").OnRead(func(ctx *web.CmsCtx, where *so.Where, _ *web.Grant) error {
		if isStaff(ctx) {
			*where = so.P("true")
			return nil
		}
		member, err := ctx.Principal()
		if err == nil && member != nil {
			*where = so.OR(
				so.P("=", "$state", "approved"),
				so.P("in", "->author", []any{member.ID}),
			)
			return nil
		}
		*where = so.P("=", "$state", "approved")
		return nil
	})
	site.Type("comment").OnCreate(func(ctx *web.CmsCtx, node *core.Node, g *web.Grant) error {
		g.Add(types.RolePublic, "body", "target", "parent")
		member, err := ctx.Principal()
		if err == nil && member != nil {
			node.Fields["author"] = member.ID
		}
		node.Fields["state"] = "pending"
		return nil
	})
}

// isStaff 夹具里的"管理角色"（association 里是 owner/admin/秘书处）。
func isStaff(ctx *web.CmsCtx) bool {
	return ctx.Actor().IsOwner() || ctx.Actor().HasRole(types.RoleAdmin) || ctx.Actor().HasRole("秘书处")
}

// create 直接用引擎建节点（绕过策略 —— 造夹具数据用）。
func (h *harness) create(t *testing.T, typeName string, fields core.Fields) int64 {
	t.Helper()
	id, err := h.site.Engine().CreateNode(nil, &core.Node{Type: typeName, Fields: fields})
	if err != nil {
		t.Fatalf("建 %s: %v", typeName, err)
	}
	return id
}

// post 调发表端点（token 为空 = 匿名）。
func (h *harness) post(t *testing.T, body map[string]any, token string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/comments", strings.NewReader(string(payload)))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	return response
}

// list 调列表端点（token 为空 = 匿名）。
func (h *harness) list(t *testing.T, params map[string]string, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	query := ""
	for key, value := range params {
		if query != "" {
			query += "&"
		}
		query += key + "=" + value
	}
	request := httptest.NewRequest(http.MethodGet, "/api/comments?"+query, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	out := map[string]any{}
	if response.Code == http.StatusOK {
		err := json.Unmarshal(response.Body.Bytes(), &out)
		if err != nil {
			t.Fatal(err)
		}
	}
	return response, out
}

// itemIDs 列表里的评论 id。
func itemIDs(t *testing.T, payload map[string]any) []int64 {
	t.Helper()
	items, _ := payload["items"].([]any)
	out := []int64{}
	for _, raw := range items {
		node, _ := raw.(map[string]any)
		id, _ := node["id"].(float64)
		out = append(out, int64(id))
	}
	return out
}

// 发表 → 列表（作者看得到自己的待审）。归属与状态由站点规则定, 客户端说了不算。
func TestPostAndList(t *testing.T) {
	h := newHarness(t, Options{})
	article := h.create(t, "article", core.Fields{"name": "行业动态"})

	response := h.post(t, map[string]any{"type": "article", "target": article, "body": "  很有价值  "}, h.token)
	if response.Code != http.StatusCreated {
		t.Fatalf("发表 = %d: %s", response.Code, response.Body.String())
	}
	var created map[string]any
	err := json.Unmarshal(response.Body.Bytes(), &created)
	if err != nil {
		t.Fatal(err)
	}
	node, _ := created["node"].(map[string]any)
	fields, _ := node["fields"].(map[string]any)
	if fields["body"] != "很有价值" {
		t.Fatalf("正文应当去掉首尾空白, 实际 %#v", fields["body"])
	}
	if author, _ := fields["author"].(float64); int64(author) != h.member {
		t.Fatalf("author 应当被规则强制成本人 %d, 实际 %v", h.member, fields["author"])
	}
	if fields["state"] != "pending" {
		t.Fatalf("初始状态应当由规则定（pending）, 实际 %v", fields["state"])
	}

	_, anonymous := h.list(t, map[string]string{"type": "article", "target": fmt.Sprint(article)}, "")
	if total, _ := anonymous["total"].(float64); total != 0 {
		t.Fatalf("匿名不该看到待审评论, 实际 total=%v", total)
	}
	_, author := h.list(t, map[string]string{"type": "article", "target": fmt.Sprint(article)}, h.token)
	if total, _ := author["total"].(float64); total != 1 {
		t.Fatalf("作者应当看到自己的待审评论, 实际 total=%v", total)
	}
	if ids := itemIDs(t, author); len(ids) != 1 {
		t.Fatalf("items 应当 1 条, 实际 %v", ids)
	}
}

// 审核通过后公众可见（可见性只有站点读规则一个来源）。
func TestApprovedVisibleToPublic(t *testing.T) {
	h := newHarness(t, Options{})
	article := h.create(t, "article", core.Fields{"name": "行业动态"})
	h.create(t, "comment", core.Fields{
		"body": "已通过的一条", "target": article, "state": "approved", "author": h.member,
	})
	h.create(t, "comment", core.Fields{
		"body": "还在审的一条", "target": article, "state": "pending", "author": h.member,
	})

	_, anonymous := h.list(t, map[string]string{"type": "article", "target": fmt.Sprint(article)}, "")
	if total, _ := anonymous["total"].(float64); total != 1 {
		t.Fatalf("匿名应当只看到 approved, 实际 total=%v", total)
	}
	_, author := h.list(t, map[string]string{"type": "article", "target": fmt.Sprint(article)}, h.token)
	if total, _ := author["total"].(float64); total != 2 {
		t.Fatalf("作者应当看到两条, 实际 total=%v", total)
	}
}

// 目标是地址（不只是 id）也能评论，且看不见的目标当 404。
func TestTargetByAddressAndVisibility(t *testing.T) {
	h := newHarness(t, Options{})
	h.create(t, "article", core.Fields{"name": "动态", "address": "news"})
	hidden := h.create(t, "event", core.Fields{"name": "活动"})

	response := h.post(t, map[string]any{"type": "article", "target": "news", "body": "地址也能定位"}, h.token)
	if response.Code != http.StatusCreated {
		t.Fatalf("用地址发表 = %d: %s", response.Code, response.Body.String())
	}
	// event 不在 Options.Types 里
	response = h.post(t, map[string]any{"type": "event", "target": hidden, "body": "换一个正文"}, h.token)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("未声明可评论的类型应当 400, 实际 %d", response.Code)
	}
	// 不存在的目标
	response = h.post(t, map[string]any{"type": "article", "target": 99999, "body": "又换一个正文"}, h.token)
	if response.Code != http.StatusNotFound {
		t.Fatalf("不存在的目标应当 404, 实际 %d: %s", response.Code, response.Body.String())
	}
}

// 默认不允许匿名: 反垃圾第一道闸是 fail-closed。
func TestAnonymousRejectedByDefault(t *testing.T) {
	h := newHarness(t, Options{})
	article := h.create(t, "article", core.Fields{"name": "动态"})
	response := h.post(t, map[string]any{"type": "article", "target": article, "body": "匿名"}, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("默认应当拒绝匿名（401）, 实际 %d", response.Code)
	}
}

// 开了匿名就能发，但蜜罐、长度、链接数仍然拦。
func TestAllowAnonymousAndSpamGuards(t *testing.T) {
	h := newHarness(t, Options{AllowAnonymous: true, MaxRunes: 10, MaxLinks: 1})
	article := h.create(t, "article", core.Fields{"name": "动态"})

	response := h.post(t, map[string]any{"type": "article", "target": article, "body": "匿名也可以"}, "")
	if response.Code != http.StatusCreated {
		t.Fatalf("开了匿名应当 201, 实际 %d: %s", response.Code, response.Body.String())
	}
	response = h.post(t, map[string]any{
		"type": "article", "target": article, "body": "机器人", "url": "http://spam.example",
	}, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("蜜罐命中应当 400, 实际 %d", response.Code)
	}
	response = h.post(t, map[string]any{"type": "article", "target": article, "body": strings.Repeat("长", 11)}, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("超长应当 400, 实际 %d", response.Code)
	}
	response = h.post(t, map[string]any{
		"type": "article", "target": article, "body": "两个链接 http://a.example 和 https://b.example",
	}, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("链接过多应当 400, 实际 %d", response.Code)
	}
	response = h.post(t, map[string]any{"type": "article", "target": article, "body": "   "}, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("空正文应当 400, 实际 %d", response.Code)
	}
}

// 频率与重复内容。
func TestRateLimitAndDuplicate(t *testing.T) {
	h := newHarness(t, Options{RateLimit: 2})
	article := h.create(t, "article", core.Fields{"name": "动态"})

	first := h.post(t, map[string]any{"type": "article", "target": article, "body": "第一条"}, h.token)
	if first.Code != http.StatusCreated {
		t.Fatalf("第一条 = %d", first.Code)
	}
	duplicate := h.post(t, map[string]any{"type": "article", "target": article, "body": "第一条"}, h.token)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("重复内容应当 409, 实际 %d: %s", duplicate.Code, duplicate.Body.String())
	}
	second := h.post(t, map[string]any{"type": "article", "target": article, "body": "第二条"}, h.token)
	if second.Code != http.StatusCreated {
		t.Fatalf("第二条 = %d", second.Code)
	}
	third := h.post(t, map[string]any{"type": "article", "target": article, "body": "第三条"}, h.token)
	if third.Code != http.StatusTooManyRequests {
		t.Fatalf("超频应当 429, 实际 %d: %s", third.Code, third.Body.String())
	}
}

// 回复: 落 parent, 且"回复的回复"归并到顶层（层级固定两层）。
func TestReplyFoldsToTopLevel(t *testing.T) {
	h := newHarness(t, Options{})
	article := h.create(t, "article", core.Fields{"name": "动态"})

	rootID := h.create(t, "comment", core.Fields{
		"body": "主评论", "target": article, "state": "approved", "author": h.member,
	})
	response := h.post(t, map[string]any{
		"type": "article", "target": article, "body": "回复主评论", "parent": rootID,
	}, h.token)
	if response.Code != http.StatusCreated {
		t.Fatalf("回复 = %d: %s", response.Code, response.Body.String())
	}
	var created map[string]any
	err := json.Unmarshal(response.Body.Bytes(), &created)
	if err != nil {
		t.Fatal(err)
	}
	node, _ := created["node"].(map[string]any)
	fields, _ := node["fields"].(map[string]any)
	replyID, _ := node["id"].(float64)
	if parent, _ := fields["parent"].(float64); int64(parent) != rootID {
		t.Fatalf("回复的 parent 应当 %d, 实际 %v", rootID, fields["parent"])
	}

	folded := h.post(t, map[string]any{
		"type": "article", "target": article, "body": "回复的回复", "parent": int64(replyID),
	}, h.token)
	if folded.Code != http.StatusCreated {
		t.Fatalf("二次回复 = %d: %s", folded.Code, folded.Body.String())
	}
	err = json.Unmarshal(folded.Body.Bytes(), &created)
	if err != nil {
		t.Fatal(err)
	}
	node, _ = created["node"].(map[string]any)
	fields, _ = node["fields"].(map[string]any)
	if parent, _ := fields["parent"].(float64); int64(parent) != rootID {
		t.Fatalf("回复的回复应当归并到主评论 %d, 实际 %v", rootID, fields["parent"])
	}

	// 不同内容下的评论不能互相回复
	other := h.create(t, "article", core.Fields{"name": "另一条"})
	crossed := h.post(t, map[string]any{
		"type": "article", "target": other, "body": "跨内容回复", "parent": rootID,
	}, h.token)
	if crossed.Code != http.StatusBadRequest {
		t.Fatalf("跨内容回复应当 400, 实际 %d", crossed.Code)
	}
}

// 列表按新到旧, 并且带上总数与分页。
func TestListOrderAndPaging(t *testing.T) {
	h := newHarness(t, Options{PageSize: 2})
	article := h.create(t, "article", core.Fields{"name": "动态"})
	for i := 1; i <= 3; i++ {
		h.create(t, "comment", core.Fields{
			"body": fmt.Sprintf("第 %d 条", i), "target": article, "state": "approved", "author": h.member,
		})
	}
	firstRecorder, first := h.list(t, map[string]string{"type": "article", "target": fmt.Sprint(article), "page": "1"}, "")
	if total, _ := first["total"].(float64); total != 3 {
		t.Fatalf("total 应当 3, 实际 %v（HTTP %d: %s）", first["total"], firstRecorder.Code, firstRecorder.Body.String())
	}
	_, second := h.list(t, map[string]string{"type": "article", "target": fmt.Sprint(article), "page": "2"}, "")
	pageOne, pageTwo := itemIDs(t, first), itemIDs(t, second)
	if len(pageOne) != 2 || len(pageTwo) != 1 {
		t.Fatalf("分页应当 2+1, 实际 %v / %v", pageOne, pageTwo)
	}
	if pageOne[0] <= pageOne[1] {
		t.Fatalf("应当新的在前, 实际 %v", pageOne)
	}
	// 客户端的其它条件只会与策略范围 AND（这里靠参数带不进新的可见性）
	_, hidden := h.list(t, map[string]string{"type": "article", "target": fmt.Sprint(article), "size": "999"}, "")
	if size, _ := hidden["size"].(float64); size != MaxPageSize {
		t.Fatalf("size 应当被夹到 %d, 实际 %v", MaxPageSize, hidden["size"])
	}
}

// 声明拼错在 Mount 当场爆（不是"评论列表永远空的"）。
func TestMountValidatesDeclaration(t *testing.T) {
	cases := []struct {
		name    string
		options Options
		want    string
	}{
		{"无可评论类型", Options{Types: []string{}}, "至少声明一个可评论类型"},
		{"类型不存在", Options{Types: []string{"nope"}}, "不存在"},
		{"评论类型不存在", Options{Types: []string{"article"}, Type: "review"}, "评论类型"},
		{"正文字段名拼错", Options{Types: []string{"article"}, BodyField: "text"}, "没有字段"},
		{"目标字段指向未声明类型", Options{Types: []string{"event"}}, "不在 Options.Types"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
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
			_, err = Mount(site, test.options)
			if err == nil {
				t.Fatalf("应当报错（%s）", test.name)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("报错应当提到 %q, 实际: %v", test.want, err)
			}
		})
	}
}
