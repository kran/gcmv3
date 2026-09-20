package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// apiSite 一个可用的站点: 会员自助内容 + 只给管理看的类型（用来验默认拒绝）。
//
// 返回站点、会员上下文（带令牌）、以及一个 request 选项（携带该令牌）。
func apiSite(t *testing.T) (*Site, *CmsCtx, func(*http.Request)) {
	t.Helper()
	site := newPolicySite(t)
	site.Type("article").OnRead(func(c *CmsCtx, where *so.Where, hide *Grant) error {
		if c.Actor().IsOwner() {
			*where = so.P("true")
			return nil
		}
		published := so.P("=", "$state", "published")
		if c.Actor().IsAnonymous() {
			hide.Add(types.RolePublic, "phone")
			*where = published
			return nil
		}
		*where = so.OR(published, so.P("ref", "->author", so.P("=", "id", c.Actor().NodeID)))
		return nil
	})
	site.Type("article").OnCreate(func(c *CmsCtx, node *core.Node, allow *Grant) error {
		if c.Actor().IsAnonymous() {
			return Unauthorized("请先登录")
		}
		allow.Add(types.RolePublic, "title")
		node.Fields["author"] = c.Actor().NodeID
		return nil
	})
	site.Type("article").OnUpdate(func(c *CmsCtx, node *core.Node, _ *core.NodePatch, allow *Grant) error {
		author, _ := node.Fields["author"].(int64)
		if author != c.Actor().NodeID {
			return Forbidden("仅作者本人可改")
		}
		allow.Add(types.RolePublic, "title")
		return nil
	})
	site.Type("article").OnDelete(func(c *CmsCtx, node *core.Node) error {
		author, _ := node.Fields["author"].(int64)
		if author != c.Actor().NodeID {
			return Forbidden("仅作者本人可删")
		}
		return nil
	})

	id, err := site.Engine().RegisterAuth(nil, "member", "email", "a@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "frontend", id)
	if err != nil {
		t.Fatal(err)
	}
	cms, _ := ctxFor(site, withBearer(token))
	return site, cms, withBearer(token)
}

// jsonDo 打一个 JSON 请求（可选带令牌）。
func jsonDo(t *testing.T, site *Site, method, target, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	for _, opt := range opts {
		opt(request)
	}
	recorder := httptest.NewRecorder()
	site.Setup().ServeHTTP(recorder, request)
	return recorder
}

// 错误响应只有一个形状: {"error": …, "code": …}(+details)。
func TestApiErrorShape(t *testing.T) {
	site, _, bearer := apiSite(t)
	// 401（匿名创建）
	got := jsonDo(t, site, http.MethodPost, "/api/nodes/article", `{"fields":{"title":"甲"}}`)
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("匿名创建 = %d %q", got.Code, got.Body.String())
	}
	for _, want := range []string{`"error":`, `"code":"unauthorized"`} {
		if !strings.Contains(got.Body.String(), want) {
			t.Fatalf("响应缺 %s: %q", want, got.Body.String())
		}
	}
	// 404（类型不存在）
	got = jsonDo(t, site, http.MethodGet, "/api/nodes/nope", "", bearer)
	if got.Code != http.StatusNotFound || !strings.Contains(got.Body.String(), `"code":"not_found"`) {
		t.Fatalf("类型不存在 = %d %q", got.Code, got.Body.String())
	}
	// 422 + details（字段白名单）
	created := createViaAPI(t, site, bearer, "甲")
	got = jsonDo(t, site, http.MethodPut, "/api/nodes/article/"+itoa(created.ID),
		`{"revision":1,"fields":{"state":"published"}}`, bearer)
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("白名单 = %d %q", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), `"details"`) || !strings.Contains(got.Body.String(), `"state"`) {
		t.Fatalf("该带字段级 details: %q", got.Body.String())
	}
}

// 创建 → 返回 201 + 掩码后的节点; 规则补的字段落库。
func TestApiCreate(t *testing.T) {
	site, cms, bearer := apiSite(t)
	got := jsonDo(t, site, http.MethodPost, "/api/nodes/article", `{"fields":{"title":"标题"}}`, bearer)
	if got.Code != http.StatusCreated {
		t.Fatalf("创建 = %d %q", got.Code, got.Body.String())
	}
	created := findNode(t, got.Body.String())
	if !strings.Contains(got.Body.String(), `"title":"标题"`) {
		t.Fatalf("节点该在响应里: %q", got.Body.String())
	}
	if created.Type != "article" {
		t.Fatalf("类型 = %q", created.Type)
	}
	// phone 不在白名单里 ⇒ 422（不是静默丢掉）
	got = jsonDo(t, site, http.MethodPost, "/api/nodes/article", `{"fields":{"title":"甲","phone":"1"}}`, bearer)
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("白名单外的字段 = %d %q", got.Code, got.Body.String())
	}
	// 严格解码: 未知字段 422/400, 不是静默忽略
	got = jsonDo(t, site, http.MethodPost, "/api/nodes/article", `{"field":{"title":"甲"}}`, bearer)
	if got.Code < 400 || got.Code >= 500 {
		t.Fatalf("未知字段该拒: %d %q", got.Code, got.Body.String())
	}
	// 规则补的 author 落库了
	stored, err := site.Engine().GetNode(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Fields["author"] != cms.Actor().NodeID {
		t.Fatalf("author 该是本人: %#v", stored.Fields)
	}
}

// 列表: 分页 + 排序 + filter（客户端条件只能收窄）+ 掩码。
func TestApiList(t *testing.T) {
	site, cms, bearer := apiSite(t)
	for _, title := range []string{"甲", "乙", "丙"} {
		createViaAPI(t, site, bearer, title)
	}
	// 自己的草稿: 看得到 3 条
	got := jsonDo(t, site, http.MethodGet, "/api/nodes/article?size=2", "", bearer)
	if got.Code != http.StatusOK {
		t.Fatalf("列表 = %d %q", got.Code, got.Body.String())
	}
	body := got.Body.String()
	for _, want := range []string{`"total":3`, `"page":1`, `"size":2`} {
		if !strings.Contains(body, want) {
			t.Fatalf("列表缺 %s: %q", want, body)
		}
	}
	// 第二页只有 1 条
	got = jsonDo(t, site, http.MethodGet, "/api/nodes/article?size=2&page=2", "", bearer)
	if !strings.Contains(got.Body.String(), `"page":2`) {
		t.Fatalf("第二页 = %q", got.Body.String())
	}
	// filter: 只能收窄
	got = jsonDo(t, site, http.MethodGet, "/api/nodes/article?filter="+url.QueryEscape(`(= $title "乙")`), "", bearer)
	if !strings.Contains(got.Body.String(), `"total":1`) || !strings.Contains(got.Body.String(), `"乙"`) {
		t.Fatalf("filter = %q", got.Body.String())
	}
	// 匿名: 草稿看不到（策略范围收窄）, 且 phone 被掩码
	anon := jsonDo(t, site, http.MethodGet, "/api/nodes/article", "")
	if !strings.Contains(anon.Body.String(), `"total":0`) {
		t.Fatalf("匿名该看到 0 条: %q", anon.Body.String())
	}
	// 发布一条后匿名看得到
	id := 0
	list, total, err := cms.List(core.NodeQuery{Type: "article"}, 0, 0)
	if err != nil || total == 0 {
		t.Fatalf("会员该看到自己的: %v %d", err, total)
	}
	id = int(list[0].ID)
	_, err = cms.Update("article", int64(id), &core.NodePatch{
		Revision: ptrInt64(list[0].Revision), Fields: core.Fields{"title": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	err = site.Engine().PatchNode(nil, int64(id), &core.NodePatch{
		Revision: ptrInt64(2), Fields: core.Fields{"state": "published", "phone": "138"}})
	if err != nil {
		t.Fatal(err)
	}
	anon = jsonDo(t, site, http.MethodGet, "/api/nodes/article", "")
	if !strings.Contains(anon.Body.String(), `"total":1`) {
		t.Fatalf("发布后匿名该看到 1 条: %q", anon.Body.String())
	}
	if strings.Contains(anon.Body.String(), "138") {
		t.Fatalf("phone 该被掩码: %q", anon.Body.String())
	}
}

// 排序: 走 core 的编译（能力来自声明）, 拼错字段 400。
func TestApiSort(t *testing.T) {
	site, _, bearer := apiSite(t)
	createViaAPI(t, site, bearer, "甲")
	got := jsonDo(t, site, http.MethodGet, "/api/nodes/article?sort=-$title", "", bearer)
	if got.Code != http.StatusOK {
		t.Fatalf("排序 = %d %q", got.Code, got.Body.String())
	}
	// 关系不可排序（能力来自声明: 引用字段没有 Sortable）⇒ 400
	got = jsonDo(t, site, http.MethodGet, "/api/nodes/article?sort=->author", "", bearer)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("关系不该可排序 = %d %q", got.Code, got.Body.String())
	}
	// 字段不存在 ⇒ 400
	got = jsonDo(t, site, http.MethodGet, "/api/nodes/article?sort=$nope", "", bearer)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("拼错字段 = %d %q", got.Code, got.Body.String())
	}
	// 超过 4 个 ⇒ 400
	got = jsonDo(t, site, http.MethodGet, "/api/nodes/article?sort=a,b,c,d,e", "", bearer)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("太多排序字段 = %d", got.Code)
	}
	// filter 拼错算符 ⇒ 400（编译期一处校验）
	got = jsonDo(t, site, http.MethodGet, "/api/nodes/article?filter="+url.QueryEscape("(adn 1 2)"), "", bearer)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("拼错算符 = %d %q", got.Code, got.Body.String())
	}
}

// 单个读: 类型不符 / 不存在 / 不可见都 404; 地址定位可用。
func TestApiView(t *testing.T) {
	site, cms, bearer := apiSite(t)
	created := createViaAPI(t, site, bearer, "甲")
	got := jsonDo(t, site, http.MethodGet, "/api/nodes/article/"+itoa(created.ID), "", bearer)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"title":"甲"`) {
		t.Fatalf("单个读 = %d %q", got.Code, got.Body.String())
	}
	// 类型不符: 拿 member 的路由打 article 的 id
	got = jsonDo(t, site, http.MethodGet, "/api/nodes/member/"+itoa(created.ID), "", bearer)
	if got.Code != http.StatusNotFound {
		t.Fatalf("类型不符该 404: %d %q", got.Code, got.Body.String())
	}
	// 不存在
	got = jsonDo(t, site, http.MethodGet, "/api/nodes/article/999999", "", bearer)
	if got.Code != http.StatusNotFound {
		t.Fatalf("不存在该 404: %d", got.Code)
	}
	// 地址定位（article 声明了 addressable; 会员给草稿设个地址）
	_, err := cms.Update("article", created.ID, &core.NodePatch{
		Revision: ptrInt64(1), Fields: core.Fields{"title": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	err = site.Engine().PatchNode(nil, created.ID, &core.NodePatch{
		Revision: ptrInt64(2), Fields: core.Fields{"address": "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	got = jsonDo(t, site, http.MethodGet, "/api/nodes/article/hello", "", bearer)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"hello"`) {
		t.Fatalf("地址定位 = %d %q", got.Code, got.Body.String())
	}
}

// 更新: 乐观锁必填 + 409 冲突; 删除 204。
func TestApiUpdateDelete(t *testing.T) {
	site, _, bearer := apiSite(t)
	created := createViaAPI(t, site, bearer, "甲")
	got := jsonDo(t, site, http.MethodPut, "/api/nodes/article/"+itoa(created.ID),
		`{"revision":1,"fields":{"title":"改过"}}`, bearer)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"改过"`) {
		t.Fatalf("更新 = %d %q", got.Code, got.Body.String())
	}
	// revision 缺了
	got = jsonDo(t, site, http.MethodPut, "/api/nodes/article/"+itoa(created.ID),
		`{"fields":{"title":"没版本"}}`, bearer)
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("缺 revision = %d %q", got.Code, got.Body.String())
	}
	// revision 过期
	got = jsonDo(t, site, http.MethodPut, "/api/nodes/article/"+itoa(created.ID),
		`{"revision":99,"fields":{"title":"过期"}}`, bearer)
	if got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), "revision_conflict") {
		t.Fatalf("过期 revision = %d %q", got.Code, got.Body.String())
	}
	// 删除
	got = jsonDo(t, site, http.MethodDelete, "/api/nodes/article/"+itoa(created.ID), "", bearer)
	if got.Code != http.StatusNoContent {
		t.Fatalf("删除 = %d %q", got.Code, got.Body.String())
	}
	got = jsonDo(t, site, http.MethodGet, "/api/nodes/article/"+itoa(created.ID), "", bearer)
	if got.Code != http.StatusNotFound {
		t.Fatalf("删掉后该 404: %d", got.Code)
	}
}

// **授权先于存在性检查**: 未注册写规则的类型不能当"这个 id 存不存在"的探测器。
//
// staff 类型没注册任何写规则 ⇒ 匿名/会员打它的任意 id 都是 401/403, 而不是
// "存在的 id 404、不存在的 id 404"这种区分不开的假象。
func TestApiAuthorizationBeforeExistence(t *testing.T) {
	site, _, bearer := apiSite(t)
	// staff 节点是存在（后台建的）, 但没人注册写规则
	staffID, err := site.Engine().CreateNode(nil, &core.Node{Type: "staff", Fields: core.Fields{"name": "员工"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{itoa(staffID), "999999"} {
		got := jsonDo(t, site, http.MethodDelete, "/api/nodes/staff/"+id, "", bearer)
		if got.Code != http.StatusForbidden {
			t.Fatalf("未注册写规则的类型该 403（id=%s）: %d %q", id, got.Code, got.Body.String())
		}
		got = jsonDo(t, site, http.MethodDelete, "/api/nodes/staff/"+id, "")
		if got.Code != http.StatusUnauthorized {
			t.Fatalf("匿名该 401（id=%s）: %d", id, got.Code)
		}
	}
}

// 别的会员的草稿: 看不到 ⇒ 写也是 404（不是 403 —— 不暴露"这行存在"）。
func TestApiWriteInvisibleIsNotFound(t *testing.T) {
	site, _, bearer := apiSite(t)
	created := createViaAPI(t, site, bearer, "别人的")
	// 换个会员身份
	otherID, err := site.Engine().RegisterAuth(nil, "member", "email", "b@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "乙"}})
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := site.Engine().CreateSession(nil, "frontend", otherID)
	if err != nil {
		t.Fatal(err)
	}
	got := jsonDo(t, site, http.MethodPut, "/api/nodes/article/"+itoa(created.ID),
		`{"revision":1,"fields":{"title":"篡改"}}`, withBearer(otherToken))
	if got.Code != http.StatusNotFound {
		t.Fatalf("别人的草稿该 404: %d %q", got.Code, got.Body.String())
	}
	stored, err := site.Engine().GetNode(created.ID)
	if err != nil || stored.Fields.Str("title") != "别人的" {
		t.Fatalf("不该被改: %#v %v", stored, err)
	}
}

// 请求体过大 ⇒ 413。
func TestApiBodyTooLarge(t *testing.T) {
	site, _, bearer := apiSite(t)
	big := `{"fields":{"title":"` + strings.Repeat("x", maxJSONBody+10) + `"}}`
	got := jsonDo(t, site, http.MethodPost, "/api/nodes/article", big, bearer)
	if got.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("过大请求体 = %d", got.Code)
	}
}

// pageParams 的夹取（垃圾输入不变成错误, 也不变成全表扫描）。
func TestPageParamsClamp(t *testing.T) {
	site := newTestSite(t)
	cms, _ := ctxFor(site)
	cases := []struct {
		target string
		page   int
		size   int
	}{
		{"/api/nodes/article", 1, defaultPageSize},
		{"/api/nodes/article?page=0&size=0", 1, defaultPageSize},
		{"/api/nodes/article?page=-5&size=-1", 1, defaultPageSize},
		{"/api/nodes/article?page=3&size=999", 3, maxPageSize},
		{"/api/nodes/article?page=99999999", maxPageNumber, defaultPageSize},
		{"/api/nodes/article?size=abc", 1, defaultPageSize},
	}
	for _, test := range cases {
		request := httptest.NewRequest(http.MethodGet, test.target, nil)
		cms.BaseContext.SetRequest(request)
		page, size := pageParams(cms)
		if page != test.page || size != test.size {
			t.Fatalf("%s: page=%d size=%d, want %d/%d", test.target, page, size, test.page, test.size)
		}
	}
}

// parsePositiveID 只认正整数（前导零、负数、别的东西都不认）。
func TestParsePositiveID(t *testing.T) {
	cases := map[string]bool{
		"1": true, "42": true, "007": true,
		"": false, "0": false, "-1": false, "1.5": false, "abc": false, "1a": false,
	}
	for raw, want := range cases {
		got, ok := parsePositiveID(raw)
		if ok != want {
			t.Fatalf("parsePositiveID(%q) = %d/%v, want %v", raw, got, ok, want)
		}
	}
}

// 客户端塞不进 Type（NodePatch 的 json:"-"）。
func TestApiPatchCannotSetType(t *testing.T) {
	site, _, bearer := apiSite(t)
	created := createViaAPI(t, site, bearer, "甲")
	got := jsonDo(t, site, http.MethodPut, "/api/nodes/article/"+itoa(created.ID),
		`{"revision":1,"type":"staff","fields":{}}`, bearer)
	if got.Code < 400 || got.Code >= 500 {
		t.Fatalf("type 该被拒（未知字段）: %d %q", got.Code, got.Body.String())
	}
}

// ── 小工具 ──

func createViaAPI(t *testing.T, site *Site, bearer func(*http.Request), title string) core.Node {
	t.Helper()
	got := jsonDo(t, site, http.MethodPost, "/api/nodes/article",
		`{"fields":{"title":"`+title+`"}}`, bearer)
	if got.Code != http.StatusCreated {
		t.Fatalf("创建 %q = %d %q", title, got.Code, got.Body.String())
	}
	return findNode(t, got.Body.String())
}

// findNode 解响应信封, 取 node（不手写 JSON 解析）。
func findNode(t *testing.T, body string) core.Node {
	t.Helper()
	var envelope struct {
		Node core.Node `json:"node"`
	}
	err := json.Unmarshal([]byte(body), &envelope)
	if err != nil {
		t.Fatalf("响应解不开 (%v): %q", err, body)
	}
	if envelope.Node.ID == 0 {
		t.Fatalf("响应里没有节点: %q", body)
	}
	return envelope.Node
}

func itoa(id int64) string {
	if id == 0 {
		return "0"
	}
	var digits []byte
	for id > 0 {
		digits = append([]byte{byte('0' + id%10)}, digits...)
		id /= 10
	}
	return string(digits)
}

// 过**真实 HTTP**（真 socket、真 URL 解析）跑一遍: 单测夹具用 Recorder 时, 查询串的
// 解析、请求体上限、响应头这些都走的是另一条路径。
func TestApiOverRealHTTP(t *testing.T) {
	site, cms, _ := apiSite(t)
	server := httptest.NewServer(site.Setup())
	defer server.Close()
	client := server.Client()

	token, err := site.Engine().CreateSession(nil, "frontend", cms.Actor().NodeID)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, target, body string) *http.Response {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		request, err := http.NewRequest(method, server.URL+target, reader)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	readBody := func(response *http.Response) string {
		t.Helper()
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	// 创建
	response := do(http.MethodPost, "/api/nodes/article", `{"fields":{"title":"真机"}}`)
	created := findNode(t, readBody(response))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("创建 = %d", response.StatusCode)
	}
	// 列表 + filter（带空格与引号的表达式, 由客户端编码）
	query := url.Values{"filter": {`(= $title "真机")`}, "size": {"5"}}
	response = do(http.MethodGet, "/api/nodes/article?"+query.Encode(), "")
	list := readBody(response)
	if response.StatusCode != http.StatusOK || !strings.Contains(list, `"total":1`) {
		t.Fatalf("列表 = %d %q", response.StatusCode, list)
	}
	// 单个
	response = do(http.MethodGet, "/api/nodes/article/"+itoa(created.ID), "")
	if body := readBody(response); response.StatusCode != http.StatusOK || !strings.Contains(body, "真机") {
		t.Fatalf("单个 = %d %q", response.StatusCode, body)
	}
	// 404 形状
	response = do(http.MethodGet, "/api/nodes/article/999999", "")
	if body := readBody(response); response.StatusCode != http.StatusNotFound || !strings.Contains(body, `"code":"not_found"`) {
		t.Fatalf("404 = %d %q", response.StatusCode, body)
	}
	// 删除
	response = do(http.MethodDelete, "/api/nodes/article/"+itoa(created.ID), "")
	_ = readBody(response)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("删除 = %d", response.StatusCode)
	}
}
