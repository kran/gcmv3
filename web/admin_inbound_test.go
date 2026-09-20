package web

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	so "github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

const inboundTypes = `
types:
  category:
    capabilities: { addressable: true }
    admin:
      display: name
      inbounds:
        - { ref: article.category, fields: [name, state] }
    fields:
      - { name: name, kind: text }
  article:
    capabilities: { addressable: true }
    admin: { display: name }
    fields:
      - { name: name, kind: text }
      - { name: state, kind: select, options: [draft, published], default: draft }
      - { name: category, kind: ref, to: category }
  member:
    capabilities:
      authentication: { roles: [秘书处] }
    fields:
      - { name: name, kind: text }
`

// 反向列表: 只列**读得到**的引用方（读规则不被绕过）, 且只读。
func TestAdminInbounds(t *testing.T) {
	site := newInboundSite(t)
	owner, _ := newMember(t, site, "inb", types.RoleOwner)

	category, err := site.Engine().CreateNode(nil, &core.Node{
		Type: "category", Fields: core.Fields{"name": "新闻动态"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 读规则: 只有已发布的可见 ⇒ 草稿那条不该出现在反向列表里
	for i, state := range []string{"published", "published", "draft"} {
		_, err = site.Engine().CreateNode(nil, &core.Node{Type: "article", Fields: core.Fields{
			"name": "文章" + strconv.Itoa(i), "state": state, "category": category,
		}})
		if err != nil {
			t.Fatal(err)
		}
	}

	response := jsonDo(t, site, http.MethodGet,
		"/admin/inbounds/category/"+strconv.FormatInt(category, 10), "", bearerOf(t, site, owner))
	if response.Code != http.StatusOK {
		t.Fatalf("反向列表 = %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `"total":2`) {
		t.Fatalf("该只列已发布的两条（草稿不进 —— 读规则生效）: %s", body)
	}
	if strings.Contains(body, "文章2") {
		t.Fatalf("草稿不该出现在反向列表里: %s", body)
	}
	if !strings.Contains(body, `"spec":"article.category"`) || !strings.Contains(body, `"label":"article"`) {
		t.Fatalf("该带上声明信息: %s", body)
	}
	// 声明的展示列要下发（前端据此渲染列, 而不是猜）
	if !strings.Contains(body, `"fields":["name","state"]`) {
		t.Fatalf("该下发要显示的列: %s", body)
	}
}

// 声明校验: 写法错 / 类型不存在 / 字段不存在 / 不是指向本类型 ⇒ 加载期报错。
func TestInboundsValidation(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"写法错", `
types:
  category:
    admin:
      inbounds: [{ ref: articlecategory }]
    fields: [{ name: name, kind: text }]
`, "类型.字段"},
		{"fields 里的字段不存在", `
types:
  category:
    admin:
      inbounds: [{ ref: article.category, fields: [nope] }]
    fields: [{ name: name, kind: text }]
  article:
    fields:
      - { name: name, kind: text }
      - { name: category, kind: ref, to: category }
`, "没有这个字段"},
		{"类型不存在", `
types:
  category:
    admin:
      inbounds: [{ ref: nope.category }]
    fields: [{ name: name, kind: text }]
`, "不存在"},
		{"字段不存在", `
types:
  category:
    admin:
      inbounds: [{ ref: article.nope }]
    fields: [{ name: name, kind: text }]
  article:
    fields: [{ name: name, kind: text }]
`, "没有字段"},
		{"不是指向本类型", `
types:
  category:
    admin:
      inbounds: [{ ref: article.author }]
    fields: [{ name: name, kind: text }]
  member:
    fields: [{ name: name, kind: text }]
  article:
    fields: [{ name: author, kind: ref, to: member }]
`, "不是指向本类型"},
	}
	for _, c := range cases {
		ts := types.New()
		err := ts.Load([]byte(c.yaml))
		if err == nil {
			t.Fatalf("%s: 该报错", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 错误信息该提到 %q, 实际 %v", c.name, c.want, err)
		}
	}
}

// newInboundSite 用自带的 types.yaml 起站点（策略必须在 Setup 之前注册 —— 与
// newPolicySite 同一套路）。
func newInboundSite(t *testing.T) *Site {
	t.Helper()
	basedir := t.TempDir()
	err := writeTypesFile(basedir, inboundTypes)
	if err != nil {
		t.Fatal(err)
	}
	site, err := Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = site.Close() })
	site.Auth().Register(AuthRealm{Name: "frontend", NodeType: "member", Default: true})
	site.Type("category").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("=", "$state", "published")
		return nil
	})
	site.Type("member").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	return site
}
