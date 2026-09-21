package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	so "github.com/kran/gcmv3/so"
)

// 建个真模板目录的站点（渲染是文件系统 + 上下文的事, 用真目录最实在）。
func renderSite(t *testing.T, templates map[string]string) (*Site, *Render) {
	t.Helper()
	basedir := t.TempDir()
	err := writeTypesFile(basedir, `
types:
  article:
    capabilities: { addressable: true }
    fields:
      - { name: title, kind: text }
`)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(basedir, "templates")
	err = os.MkdirAll(dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range templates {
		err = os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
		if err != nil {
			t.Fatal(err)
		}
	}
	site, err := Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = site.Close() })
	render, err := NewRender(site, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return site, render
}

func renderOf(t *testing.T, render *Render, site *Site, candidates []string, data any) string {
	t.Helper()
	ctx, _ := ctxFor(site)
	var out strings.Builder
	err := render.Render(ctx, &out, candidates, data)
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// 级联: node--{type}.html 优先, 退到 node.html。
func TestRenderCascade(t *testing.T) {
	site, render := renderSite(t, map[string]string{
		"node--article.html": "ARTICLE:{{ .Title }}",
		"node.html":          "NODE:{{ .Title }}",
	})
	if got := renderOf(t, render, site, []string{"node--article.html", "node.html"}, map[string]any{"Title": "甲"}); got != "ARTICLE:甲" {
		t.Fatalf("该用类型专属模板: %q", got)
	}
	if got := renderOf(t, render, site, []string{"node--page.html", "node.html"}, map[string]any{"Title": "甲"}); got != "NODE:甲" {
		t.Fatalf("没有专属模板该退回 node.html: %q", got)
	}
	// 候选都不存在 ⇒ 响亮报错（不静默空页）
	ctx, _ := ctxFor(site)
	var out strings.Builder
	err := render.Render(ctx, &out, []string{"nope.html"}, nil)
	if err == nil || !strings.Contains(err.Error(), "找不到模板") {
		t.Fatalf("缺模板该报错: %v", err)
	}
}

// 内置函数: rich/excerpt/date/default/join/asset + partial/partialOr。
func TestRenderBuiltins(t *testing.T) {
	site, render := renderSite(t, map[string]string{
		"page.html": `{{ rich .Body }}|{{ excerpt .Body 4 }}|{{ date .At }}|{{ .Missing | default "—" }}|{{ join "," .Tags }}|{{ url "uploads/a.png" }}|{{ partial "card.html" . }}`,
		"card.html": "CARD:{{ .Title }}",
	})
	render.base = "https://cdn.example.com"
	data := map[string]any{
		"Title": "标题", "Body": "<p>新能源产业对接</p>", "At": int64(1784367000),
		"Tags": []string{"a", "b"},
	}
	got := renderOf(t, render, site, []string{"page.html"}, data)
	for _, want := range []string{
		"<p>新能源产业对接</p>",                        // rich: 不转义
		"新能源产…",                                 // excerpt: 按字符截断（4 个字符 + 省略号）
		"2026-07-18",                            // date: Unix 秒 → 日期
		"—",                                     // default: 空值兜底
		"a,b",                                   // join
		"https://cdn.example.com/uploads/a.png", // url: 带前缀
		"CARD:标题",                               // partial
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("渲染结果少了 %q:\n%s", want, got)
		}
	}
}

// 站点注册的函数: 带 *CmsCtx 的自动注入上下文（框架唯一给的便利）。
func TestRenderCustomFuncWithContext(t *testing.T) {
	site, render := renderSite(t, map[string]string{
		"page.html": `{{ hello "世界" }}|{{ withoutctx "x" }}`,
	})
	err := render.Func("hello", func(_ *CmsCtx, who string) (string, error) {
		return "你好 " + who, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = render.Func("withoutctx", func(value string) (string, error) { return strings.ToUpper(value), nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := renderOf(t, render, site, []string{"page.html"}, nil); got != "你好 世界|X" {
		t.Fatalf("自定义函数: %q", got)
	}

	// 签名不对 ⇒ 注册期就报（不留到渲染）
	err = render.Func("bad", func() string { return "" })
	if err == nil {
		t.Fatal("签名不合法该报错（要是 func(...) (值, error)）")
	}
	// 模板里用了没注册的函数 ⇒ 渲染期报错（fail-loud, 比 sprig 静默空值强）
	_, render2 := renderSite(t, map[string]string{"p.html": `{{ nope "x" }}`})
	ctx, _ := ctxFor(site)
	var out strings.Builder
	err = render2.Render(ctx, &out, []string{"p.html"}, nil)
	if err == nil {
		t.Fatal("未注册的函数该渲染期报错")
	}
}

// ServeNode: ref（id 或地址）→ 读入口 → 级联 → 404。
func TestRenderServeNode(t *testing.T) {
	site, render := renderSite(t, map[string]string{
		"node--article.html": "A:{{ .Node.Fields.title }}|{{ .Path }}",
		"node.html":          "N:{{ .Node.Fields.title }}",
		"404.html":           "没找到: {{ .Path }}",
	})
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	id, err := site.Engine().CreateNode(nil, &core.Node{
		Type: "article", Fields: core.Fields{"title": "标题", "address": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, rec := ctxFor(site, func(r *http.Request) { r.URL.Path = "/node/hello" })
	if err := render.ServeNode(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "A:标题|/node/hello" {
		t.Fatalf("按 id 渲染: %q", got)
	}
	// 按地址也一样（地址全表唯一 ⇒ 不需要类型）
	ctx, rec = ctxFor(site, func(r *http.Request) { r.URL.Path = "/node/hello" })
	if err := render.ServeNode(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "A:标题|/node/hello" {
		t.Fatalf("按地址渲染: %q", got)
	}

	// 读不到 ⇒ 404 状态 + 404.html（不泄漏存在性）
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("false")
		return nil
	})
	// 读规则是配置期的（Setup 之前）—— 这里换不了, 于是用另一个没有读规则的类型来验 404
	ctx, rec = ctxFor(site, func(r *http.Request) { r.URL.Path = "/node/nope" })
	if err := render.ServeNode(ctx, "nope"); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("找不到该 404, 实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "没找到: /node/nope") {
		t.Fatalf("该渲染 404.html: %q", rec.Body.String())
	}
	// url(node) 缺 address ⇒ 报错（不静默空 href）
	node := &core.Node{ID: 7, Type: "article", Fields: core.Fields{"title": "无地址"}}
	if _, err := render.url(node); err == nil {
		t.Fatal("没有 address 的节点拼 URL 该报错")
	}
}
