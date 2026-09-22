package settings

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	so "github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
	"github.com/kran/gcmv3/web"
)

// 夹具: 真站点（真 sqlite 文件）+ 装好的配置插件 + 一个 owner 令牌。
func newHarness(t *testing.T, options Options) (*web.Site, http.Handler, string, *Plugin) {
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
		{Key: "site.phone", Kind: KindText, Label: "联系电话"},
		{Key: "site.logo", Kind: KindUploadImage, Label: "站点 Logo"},
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

// 写 / 读 / 清（清 = 回默认）。
func TestSetGetClear(t *testing.T) {
	_, _, _, plugin := newHarness(t, Options{})
	err := plugin.Set("site.phone", "020-1234")
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
	err = plugin.Clear("site.phone")
	if err != nil {
		t.Fatal(err)
	}
	if got := plugin.String("site.phone"); got != "" {
		t.Fatalf("清空后该回到默认（空串）, 实际 %q", got)
	}
	// 读一个没声明的键 ⇒ 报错（不是静默空值: 拼错键名这种错最难查）
	if _, err := plugin.All(); err != nil {
		t.Fatal(err)
	}
	err = plugin.Set("nope.key", "x")
	if !errors.Is(err, ErrUndeclared) {
		t.Fatalf("写未声明的键该 ErrUndeclared, 实际 %v", err)
	}
	var out string
	_, err = plugin.Get("nope.key", &out)
	if !errors.Is(err, ErrUndeclared) {
		t.Fatalf("读未声明的键该 ErrUndeclared, 实际 %v", err)
	}
}

// 值必须符合声明的形态（fail-loud: number 配置存不了 "abc"）。
func TestKindValidation(t *testing.T) {
	_, _, _, plugin := newHarness(t, Options{})
	cases := []struct {
		key   string
		value any
	}{
		{"site.counter", "abc"},
		{"site.open", "yes"},
		{"site.mode", "半"},
		{"site.name", 42},
		{"site.logo", []any{"a"}},
	}
	for _, one := range cases {
		err := plugin.Set(one.key, one.value)
		if err == nil {
			t.Fatalf("%s 存 %#v 该被拒", one.key, one.value)
		}
	}
	// 合法值照常
	err := plugin.Set("site.counter", 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := plugin.Int("site.counter"); got != 7 {
		t.Fatalf("数字该写进去, 实际 %d", got)
	}
	err = plugin.Set("site.mode", "全")
	if err != nil {
		t.Fatal(err)
	}
	// json 形态任意值都可以
	err = plugin.Set("site.extra", map[string]any{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
}

// 声明本身有问题 ⇒ Mount 当场报错（不留到后台点开才发现）。
func TestMountValidatesDeclaration(t *testing.T) {
	cases := []struct {
		name  string
		items []Item
		want  string
	}{
		{"空清单", []Item{}, "至少声明一条"},
		{"键为空", []Item{{Key: "", Kind: KindText}}, "不能为空"},
		{"键非法", []Item{{Key: "site name", Kind: KindText}}, "只能由"},
		{"没写 Kind", []Item{{Key: "a", Kind: ""}}, "没写 Kind"},
		{"Kind 不认识", []Item{{Key: "a", Kind: "wysiwyg"}}, "不认识"},
		{"select 没有候选", []Item{{Key: "a", Kind: KindSelect}}, "没给 Options"},
		{"默认值不合形态", []Item{{Key: "a", Kind: KindNumber, Default: "x"}}, "Default 不合法"},
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

// 后台端点: 没登录 ⇒ 401/403; owner ⇒ 200/204; 未声明的键 ⇒ 404。
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
		t.Fatalf("清单该是声明的 %d 条, 实际 %d", len(testItems()), len(list.Items))
	}
	if list.Items[0].Key != "site.name" || list.Items[0].Value != "商协会" {
		t.Fatalf("清单该按声明顺序并带回默认值: %#v", list.Items[0])
	}

	set := do(t, handler, http.MethodPut, "/admin/settings/site.phone", token, `{"value":"020-9999"}`)
	if set.Code != http.StatusNoContent {
		t.Fatalf("写 = %d: %s", set.Code, set.Body.String())
	}
	bad := do(t, handler, http.MethodPut, "/admin/settings/site.counter", token, `{"value":"abc"}`)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("值不合形态该 400, 实际 %d: %s", bad.Code, bad.Body.String())
	}
	unknown := do(t, handler, http.MethodPut, "/admin/settings/nope.key", token, `{"value":"x"}`)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("未声明的键该 404, 实际 %d: %s", unknown.Code, unknown.Body.String())
	}
	clear := do(t, handler, http.MethodDelete, "/admin/settings/site.phone", token, "")
	if clear.Code != http.StatusNoContent {
		t.Fatalf("清空 = %d: %s", clear.Code, clear.Body.String())
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
	denied := do(t, handler, http.MethodPut, "/admin/settings/site.name", clerkToken, `{"value":"改名"}`)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("默认只有 owner 能改配置, 实际 %d: %s", denied.Code, denied.Body.String())
	}
	allowed := do(t, handler, http.MethodPut, "/admin/settings/site.name", ownerToken, `{"value":"新名字"}`)
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("owner 该能改, 实际 %d", allowed.Code)
	}
}
