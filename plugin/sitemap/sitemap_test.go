package sitemap

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	so "github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/web"
)

const testTypes = `
types:
  article:
    capabilities: { addressable: true }
    fields:
      - { name: name, kind: text }
      - { name: state, kind: select, options: [draft, published], default: draft }
  event:
    capabilities: { addressable: true }
    fields:
      - { name: name, kind: text }
      - { name: state, kind: select, options: [draft, published], default: draft }
  banner:
    fields:
      - { name: name, kind: text }
`

func newSite(t *testing.T) *web.Site {
	t.Helper()
	basedir := t.TempDir()
	err := os.WriteFile(filepath.Join(basedir, "types.yaml"), []byte(testTypes), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	site, err := web.Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = site.Close() })
	// 读规则: 只有已发布可见（用来证明 sitemap 不绕过读规则）
	for _, name := range []string{"article", "event"} {
		site.Type(name).OnRead(func(_ *web.CmsCtx, where *so.Where, _ *web.Grant) error {
			*where = so.P("=", "$state", "published")
			return nil
		})
	}
	return site
}

func create(t *testing.T, site *web.Site, typeName string, fields core.Fields) int64 {
	t.Helper()
	id, err := site.Engine().CreateNode(nil, &core.Node{Type: typeName, Fields: fields})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// 只收**可读**的节点（读规则照旧生效 —— sitemap 不是旁路）。
func TestSitemapRespectsReadRules(t *testing.T) {
	site := newSite(t)
	_, err := Mount(site, Options{Types: []string{"article"}, BaseURL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	create(t, site, "article", core.Fields{"name": "已发布的", "state": "published", "address": "published-one"})
	create(t, site, "article", core.Fields{"name": "草稿", "state": "draft", "address": "draft-one"})

	response := httptest.NewRecorder()
	site.Setup().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sitemap.xml", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("sitemap = %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "https://example.com/published-one") {
		t.Fatalf("该有一条绝对 URL: %s", body)
	}
	if strings.Contains(body, "draft-one") {
		t.Fatalf("草稿不该进 sitemap（读规则生效）: %s", body)
	}
	if !strings.HasPrefix(body, "<?xml") || !strings.Contains(body, "<urlset") {
		t.Fatalf("该是 XML: %s", body)
	}
	if !strings.Contains(response.Header().Get("Content-Type"), "xml") {
		t.Fatalf("Content-Type 该是 xml: %q", response.Header().Get("Content-Type"))
	}
}

// 多个类型合并 + 自定义路径。
func TestSitemapMultipleTypesAndPath(t *testing.T) {
	site := newSite(t)
	_, err := Mount(site, Options{Types: []string{"article", "event"}, BaseURL: "https://x.org/",
		Path: "/sitemap-news.xml"})
	if err != nil {
		t.Fatal(err)
	}
	create(t, site, "article", core.Fields{"name": "文章", "state": "published", "address": "a1"})
	create(t, site, "event", core.Fields{"name": "活动", "state": "published", "address": "e1"})

	response := httptest.NewRecorder()
	site.Setup().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sitemap-news.xml", nil))
	body := response.Body.String()
	if !strings.Contains(body, "https://x.org/a1") || !strings.Contains(body, "https://x.org/e1") {
		t.Fatalf("两个类型都该在: %s", body)
	}
}

// fail-loud: 配错当场报（类型不存在 / 类型没有 addressable / 没有 BaseURL）。
func TestMountFailsLoud(t *testing.T) {
	site := newSite(t)
	cases := []struct {
		options Options
		want    string
	}{
		{Options{Types: []string{"article"}}, "BaseURL"},
		{Options{Types: []string{"nope"}, BaseURL: "https://x.org"}, "nope"},
		{Options{Types: []string{"banner"}, BaseURL: "https://x.org"}, "addressable"},
		{Options{BaseURL: "https://x.org"}, "至少声明"},
	}
	for _, c := range cases {
		_, err := Mount(site, c.options)
		if err == nil {
			t.Fatalf("%#v 该报错", c.options)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("错误信息该提到 %q: %v", c.want, err)
		}
	}
}
