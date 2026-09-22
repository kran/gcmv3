package settings

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	so "github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
	"github.com/kran/gcmv3/web"
)

// 夹具: 真站点（真 sqlite 文件）+ 装好的配置插件 + 一个 owner 令牌。
//
// prepare 在**挂插件之前**跑 —— 造老库（建旧表/塞旧数据）只能在这个时点。
func newHarness(t *testing.T, options Options, prepare ...func(*web.Site)) (*web.Site, http.Handler, string, *Plugin) {
	t.Helper()
	basedir := t.TempDir()
	err := os.WriteFile(filepath.Join(basedir, "site.yaml"), []byte(`
types:
  staff:
    capabilities:
      authentication: { roles: [秘书处] }
    fields:
      - { name: name, kind: text }
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	site, err := web.Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = site.Close() })
	site.Type("staff").OnRead(func(_ *web.CmsCtx, where *so.Where, _ *web.Grant) error {
		*where = so.P("true")
		return nil
	})

	if options.Items == nil {
		options.Items = testItems()
	}
	for _, fn := range prepare {
		fn(site)
	}
	plugin, err := Mount(site, options)
	if err != nil {
		t.Fatal(err)
	}

	staffID, err := site.Engine().RegisterAuth(nil, "staff", "username", "boss",
		core.Fields{}, &core.Node{Type: "staff",
			Fields: core.Fields{"name": "站长", "roles": []any{types.RoleOwner}}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "staff", staffID)
	if err != nil {
		t.Fatal(err)
	}
	return site, site.Setup(), token, plugin
}

func testItems() []Item {
	return []Item{
		{Key: "site.name", Kind: KindText, Label: "站点名称", Default: "商协会"},
		{Key: "site.phone", Kind: KindText, Label: "联系电话", Group: "site"},
		{Key: "site.logo", Kind: KindUploadImage, Label: "站点 Logo", Group: "site"},
		{Key: "site.counter", Kind: KindNumber, Label: "计数", Default: int64(3)},
		{Key: "site.open", Kind: KindBool, Label: "是否开放", Default: true},
		{Key: "site.mode", Kind: KindSelect, Label: "模式", Options: []string{"简", "全"}},
		{Key: "site.extra", Kind: KindJSON, Label: "扩展"},
	}
}

func do(t *testing.T, handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// 未设置的键读声明里的 Default —— 站点代码不用到处写"如果没配就用…"。
func TestDefaults(t *testing.T) {
	site, _, _, plugin := newHarness(t, Options{})
	_ = site
	if got := plugin.String("site.name"); got != "商协会" {
		t.Fatalf("默认值该是声明里的 商协会, 实际 %q", got)
	}
	if got := plugin.Int("site.counter"); got != 3 {
		t.Fatalf("默认数字该是 3, 实际 %d", got)
	}
	if !plugin.Bool("site.open") {
		t.Fatal("默认布尔该是 true")
	}
	if got := plugin.String("site.phone"); got != "" {
		t.Fatalf("没声明默认值 ⇒ 空串, 实际 %q", got)
	}
}

// 写 / 读 / 删（删 = 回到预置的 Default, 没有预置就是"没值"）。
func TestSetGetDelete(t *testing.T) {
	_, _, _, plugin := newHarness(t, Options{})
	err := plugin.Set("site.phone", "site", KindText, "020-1234")
	if err != nil {
		t.Fatal(err)
	}
	if got := plugin.String("site.phone"); got != "020-1234" {
		t.Fatalf("读回该是写入的值, 实际 %q", got)
	}
	all, err := plugin.All()
	if err != nil {
		t.Fatal(err)
	}
	if all["site.phone"] != "020-1234" || all["site.name"] != "商协会" {
		t.Fatalf("All() 该含写入值与默认值: %#v", all)
	}

	// **自由键**: 没预置过的键照样能写能读（v2 那套的关键差别）
	err = plugin.Set("footer.text", "site", KindTextarea, "© 2026 商协会")
	if err != nil {
		t.Fatal(err)
	}
	if got := plugin.String("footer.text"); got != "© 2026 商协会" {
		t.Fatalf("自由键该读写自如, 实际 %q", got)
	}

	err = plugin.Delete("site.phone")
	if err != nil {
		t.Fatal(err)
	}
	if got := plugin.String("site.phone"); got != "" {
		t.Fatalf("删掉后该回到预置默认（空串）, 实际 %q", got)
	}

	// 删不存在的 ⇒ 报错（不是静默成功）
	err = plugin.Delete("nope.key")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("删不存在的键该 ErrNotFound, 实际 %v", err)
	}

	// 既没预置也没写过 ⇒ 静默无值（自由 KV 的代价, 文档里写了）
	var out string
	ok, err := plugin.Get("nope.key", &out)
	if err != nil || ok {
		t.Fatalf("没预置也没写过的键该 (false, nil), 实际 (%v, %v)", ok, err)
	}
}

// **服务端不管值与类型**（v2 的 SetSetting 也不管）: 类型只是后台选控件的提示,
// 写进去什么就是什么 —— 只拦键格式与编不出来的值。
func TestNoValueOrTypeValidation(t *testing.T) {
	_, _, _, plugin := newHarness(t, Options{})
	// 类型名随便写（库里存了别的名字也能存能读, 只是后台找不到控件）
	values := []struct {
		key, typ string
		value    any
	}{
		{"site.counter", KindNumber, "abc"},   // 与预置的形态不符也存
		{"site.open", KindBool, "yes"},        //
		{"site.mode", KindSelect, "不在候选里"},    // 预置的候选项也不拿来卡写入
		{"custom.x", "wysiwyg", "任意"},         // 类型名字没见过
		{"custom.y", "", 42},                  // 空类型
		{"custom.z", KindObject, []any{1, 2}}, // 结构不对口
	}
	for _, one := range values {
		err := plugin.Set(one.key, "", one.typ, one.value)
		if err != nil {
			t.Fatalf("%s 存 %#v 该原样存下（v2 不校验）, 实际 %v", one.key, one.value, err)
		}
	}
	if got := plugin.String("site.counter"); got != "abc" {
		t.Fatalf("存了什么就读回什么, 实际 %q", got)
	}
	// 键格式还是拦的（v2 的 checkKey）
	err := plugin.Set("bad key", "", KindText, "x")
	if err == nil {
		t.Fatal("键里有空格该被拒")
	}
}

// 一条预置都不写也照装（纯自由 KV）; 类型以**库里的列**为准, 不被预置改回去。
func TestFreeKeysAndTypes(t *testing.T) {
	site, handler, token, plugin := newHarness(t, Options{Items: []Item{}})
	_ = site
	err := plugin.Set("ad-hoc", "杂项", KindNumber, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := plugin.Int("ad-hoc"); got != 5 {
		t.Fatalf("自由键该读回 5, 实际 %d", got)
	}
	// 后台列表带回分组与类型（面板按它们渲染控件）
	list := do(t, handler, http.MethodGet, "/admin/settings", token, "")
	if list.Code != http.StatusOK {
		t.Fatalf("列表 = %d: %s", list.Code, list.Body.String())
	}
	var body struct {
		Items []settingItem `json:"items"`
	}
	err = json.Unmarshal(list.Body.Bytes(), &body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].Key != "ad-hoc" || body.Items[0].Group != "杂项" ||
		body.Items[0].Type != KindNumber {
		t.Fatalf("清单该带回库里的分组/类型: %#v", body.Items)
	}
}

// 预置声明本身有问题 ⇒ Mount 当场报错（不留到后台点开才发现）。
func TestMountValidatesDeclaration(t *testing.T) {
	cases := []struct {
		name  string
		items []Item
		want  string
	}{
		{"键为空", []Item{{Key: "", Kind: KindText}}, "不能为空"},
		{"键非法", []Item{{Key: "site name", Kind: KindText}}, "只能由"},
		{"没写 Kind", []Item{{Key: "a", Kind: ""}}, "没写 Kind"},
		{"select 没有候选", []Item{{Key: "a", Kind: KindSelect}}, "没给 Options"},
		{"重复键", []Item{{Key: "a", Kind: KindText}, {Key: "a", Kind: KindText}}, "两次"},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			basedir := t.TempDir()
			err := os.WriteFile(filepath.Join(basedir, "site.yaml"), []byte("types:\n  staff:\n    capabilities:\n      authentication: { roles: [秘书处] }\n    fields:\n      - { name: name, kind: text }\n"), 0o600)
			if err != nil {
				t.Fatal(err)
			}
			site, err := web.Open(basedir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = site.Close() })
			_, err = Mount(site, Options{Items: one.items})
			if err == nil || !strings.Contains(err.Error(), one.want) {
				t.Fatalf("该报 %q, 实际 %v", one.want, err)
			}
		})
	}
}

// 后台端点: 没登录 ⇒ 401/403; owner ⇒ 200/204; 任意键能建; 值不合形态 ⇒ 400。
func TestEndpoints(t *testing.T) {
	_, handler, token, _ := newHarness(t, Options{})

	anon := do(t, handler, http.MethodGet, "/admin/settings", "", "")
	if anon.Code == http.StatusOK {
		t.Fatalf("没登录不该能读配置, 实际 %d", anon.Code)
	}

	link := do(t, handler, http.MethodGet, "/admin/settings", token, "")
	if link.Code != http.StatusOK {
		t.Fatalf("列表 = %d: %s", link.Code, link.Body.String())
	}
	var list struct {
		Items []settingItem `json:"items"`
	}
	err := json.Unmarshal(link.Body.Bytes(), &list)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != len(testItems()) {
		t.Fatalf("清单该是预置的 %d 条, 实际 %d", len(testItems()), len(list.Items))
	}
	if list.Items[0].Key != "site.name" || list.Items[0].Value != "商协会" {
		t.Fatalf("清单该按预置顺序并带回默认值: %#v", list.Items[0])
	}
	if list.Items[0].UpdatedAt != 0 {
		t.Fatalf("没写过的项 updated_at 该是 0（面板据此显示默认值）: %d", list.Items[0].UpdatedAt)
	}

	set := do(t, handler, http.MethodPost, "/admin/settings", token,
		`{"key":"site.phone","group":"site","type":"text","value":"020-9999"}`)
	if set.Code != http.StatusNoContent {
		t.Fatalf("写 = %d: %s", set.Code, set.Body.String())
	}
	// 没预置过的键也建得成（v2 那套的关键差别）
	free := do(t, handler, http.MethodPost, "/admin/settings", token,
		`{"key":"footer.icp","group":"seo","type":"text","value":"粤ICP备 123"}`)
	if free.Code != http.StatusNoContent {
		t.Fatalf("自由键该能建, 实际 %d: %s", free.Code, free.Body.String())
	}

	// 值/类型不合预置也不拦（v2 不校验）—— 后台表单写下来的就是库里的
	loose := do(t, handler, http.MethodPost, "/admin/settings", token,
		`{"key":"site.counter","type":"number","value":"abc"}`)
	if loose.Code != http.StatusNoContent {
		t.Fatalf("值不合预置的形态也该存下, 实际 %d: %s", loose.Code, loose.Body.String())
	}
	badKey := do(t, handler, http.MethodPost, "/admin/settings", token,
		`{"key":"bad key","type":"text","value":"x"}`)
	if badKey.Code != http.StatusBadRequest {
		t.Fatalf("键不合法该 400, 实际 %d: %s", badKey.Code, badKey.Body.String())
	}

	del := do(t, handler, http.MethodDelete, "/admin/settings/site.phone", token, "")
	if del.Code != http.StatusNoContent {
		t.Fatalf("删 = %d: %s", del.Code, del.Body.String())
	}
	missing := do(t, handler, http.MethodDelete, "/admin/settings/nope.key", token, "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("删不存在的该 404, 实际 %d: %s", missing.Code, missing.Body.String())
	}
}

// 老库（v3 初版的三列表）直接能用: 补列 + 用预置的形态回填 —— 不丢数据, 也不会把一条
// 富文本变成用多行文本框编辑。
func TestMigratesLegacyTable(t *testing.T) {
	prepare := func(site *web.Site) {
		_, err := site.DB().Add(`CREATE TABLE settings (
			key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL)`).Exec()
		if err != nil {
			t.Fatal(err)
		}
		_, err = site.DB().Add(`INSERT INTO settings (key, value, updated_at) VALUES
			('about-us', '"<p>关于我们</p>"', 1700000000),
			('legacy.key', '"普通字符串"', 1700000001)`).Exec()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, handler, token, plugin := newHarness(t, Options{Items: []Item{
		{Key: "about-us", Kind: KindRichtext, Label: "首页简介", Group: "home"},
	}}, prepare)

	if got := plugin.String("about-us"); got != "<p>关于我们</p>" {
		t.Fatalf("老数据该原样读得到, 实际 %q", got)
	}
	list := do(t, handler, http.MethodGet, "/admin/settings", token, "")
	if list.Code != http.StatusOK {
		t.Fatalf("列表 = %d: %s", list.Code, list.Body.String())
	}
	var body struct {
		Items []settingItem `json:"items"`
	}
	err := json.Unmarshal(list.Body.Bytes(), &body)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]settingItem{}
	for _, one := range body.Items {
		byKey[one.Key] = one
	}
	// 预置声明的形态回填到刚补出来的列上
	if got := byKey["about-us"]; got.Type != KindRichtext || got.Group != "home" {
		t.Fatalf("补列后该用预置的形态回填, 实际 type=%q group=%q", got.Type, got.Group)
	}
	// 没预置的老行: 补列给的是占位值（text/未分组）—— 数据还在, 形态可以在后台改
	if got := byKey["legacy.key"]; got.Type != KindText || got.Value != "普通字符串" {
		t.Fatalf("没预置的老行该保留数据 + text 占位, 实际 %#v", got)
	}
}

// 面板里的端点前缀由 Go 侧注入（Options.Prefix 改了也不会打到默认地址上）。
func TestPanelPrefixInjected(t *testing.T) {
	_, handler, token, _ := newHarness(t, Options{Prefix: "/conf"})
	got := do(t, handler, http.MethodGet, "/admin/conf/panel.vue", token, "")
	if got.Code != http.StatusOK {
		t.Fatalf("面板 = %d: %s", got.Code, got.Body.String())
	}
	body := got.Body.String()
	if strings.Contains(body, "__PREFIX__") {
		t.Fatal("面板里的 __PREFIX__ 没被替换")
	}
	if !strings.Contains(body, "'/admin/conf'") {
		t.Fatalf("面板该拿到实际前缀: %s", body[:min(len(body), 400)])
	}
}

// 面板的类型单选就是 v2 表单里那 8 个（与 Go 侧无关 —— 服务端不校验类型,
// 面板也不该拿 Go 的常量清单去卡）。
func TestPanelKindListIsV2Form(t *testing.T) {
	data, err := panelFS.ReadFile("web/settings.vue")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`const KINDS = \[([^\]]*)\]`).FindStringSubmatch(string(data))
	if match == nil {
		t.Fatal("面板里找不到 const KINDS 清单（改名了？）")
	}
	var got []string
	for _, quoted := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(match[1], -1) {
		got = append(got, quoted[1])
	}
	want := []string{"text", "textarea", "number", "bool", "object", "array", "upload-file", "richtext"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("面板的类型单选该是 v2 那 8 个 %v, 实际 %v", want, got)
	}
}

// Roles 之外的账号不能改（默认只有 owner）。
func TestRolesGuard(t *testing.T) {
	site, handler, ownerToken, _ := newHarness(t, Options{})
	// 一个只有 秘书处 角色、不是 owner 的账号
	staffID, err := site.Engine().RegisterAuth(nil, "staff", "username", "clerk",
		core.Fields{}, &core.Node{Type: "staff",
			Fields: core.Fields{"name": "文员", "roles": []any{"秘书处"}}})
	if err != nil {
		t.Fatal(err)
	}
	clerkToken, err := site.Engine().CreateSession(nil, "staff", staffID)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"key":"site.name","type":"text","value":"改名"}`
	denied := do(t, handler, http.MethodPost, "/admin/settings", clerkToken, body)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("默认只有 owner 能改配置, 实际 %d: %s", denied.Code, denied.Body.String())
	}
	allowed := do(t, handler, http.MethodPost, "/admin/settings", ownerToken, body)
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("owner 该能改, 实际 %d", allowed.Code)
	}
}
