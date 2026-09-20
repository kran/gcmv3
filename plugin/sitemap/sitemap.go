// Package sitemap 站点地图插件 —— 挂一条公开的 /sitemap.xml。
//
//	sitemap.Mount(site, sitemap.Options{
//	    Types:   []string{"article", "event", "page"},
//	    BaseURL: "https://example.com",
//	})
//
// 三条设计:
//
//  1. **走受管读入口**（ctx.List）—— 读规则照旧生效: 读不到的行不进 sitemap。
//     这是检索/列表之外的又一个"能被绕过"的入口, 所以它不自己查库。
//  2. **只收有 address 的类型**: loc 必须是稳定 URL, 而地址是可寻址类型才有的
//     （全表唯一）。类型没声明 addressable ⇒ Mount 当场报错, 不静默少一半内容。
//  3. 公开端点（搜索引擎要能取）—— 不挂后台门; 内容里没有秘密（受管读入口保证）。
package sitemap

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/web"
)

// DefaultPath 默认挂载路径。
const DefaultPath = "/sitemap.xml"

// pageSize 每次读多少节点（走 ctx.List 分页, 不受 /api 的 size 上限约束）。
const pageSize = 200

// Options 插件配置。
type Options struct {
	// Types 要进 sitemap 的类型（必须声明 addressable 能力）。
	Types []string
	// BaseURL 站点对外地址（sitemap 要求绝对 URL —— 相对路径不合规范）。
	BaseURL string
	// Path 挂载路径（空 = DefaultPath）。
	Path string
}

// Plugin 装好的插件。
type Plugin struct {
	site    *web.Site
	types   []string
	baseURL string
	path    string
}

// Mount 装上插件并挂出 /sitemap.xml。
func Mount(site *web.Site, options Options) (*Plugin, error) {
	if site == nil {
		return nil, fmt.Errorf("sitemap: site is required")
	}
	base := strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("sitemap: 需要 BaseURL（sitemap 里的 loc 必须是绝对 URL）")
	}
	if len(options.Types) == 0 {
		return nil, fmt.Errorf("sitemap: 至少声明一个类型（Types 为空 —— 装了也是空文件）")
	}
	plugin := &Plugin{site: site, baseURL: base, path: strings.TrimSpace(options.Path)}
	if plugin.path == "" {
		plugin.path = DefaultPath
	}
	for _, name := range options.Types {
		name = strings.TrimSpace(name)
		def, ok := site.Types().Type(name)
		if !ok {
			return nil, fmt.Errorf("sitemap: 类型 %q 不存在（Types 拼错了？）", name)
		}
		if !def.Capabilities.Addressable {
			return nil, fmt.Errorf(
				"sitemap: 类型 %q 没有 addressable 能力 —— 它没有稳定地址, 放不进 sitemap（要么去掉它, 要么给类型加 capabilities.addressable）", name)
		}
		plugin.types = append(plugin.types, name)
	}
	site.Hook(web.HookBeforeMount, func(s *web.Site) error {
		s.Router().Get(plugin.path, plugin.serve)
		return nil
	})
	return plugin, nil
}

// urlset / url 是 sitemap 协议的形状（lastmod 用 W3C 日期）。
type urlset struct {
	XMLName xml.Name `xml:"urlset"`
	Xmlns   string   `xml:"xmlns,attr"`
	URLs    []url    `xml:"url"`
}

type url struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

// serve GET /sitemap.xml
func (p *Plugin) serve(ctx *web.CmsCtx) {
	set := urlset{Xmlns: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	for _, typeName := range p.types {
		nodes, err := p.collect(ctx, typeName)
		if err != nil {
			ctx.Fail(err)
			return
		}
		for _, node := range nodes {
			if node.Address == nil || *node.Address == "" {
				continue // 没地址的节点不进（地址由 addressable 能力保证, 这里是兜底）
			}
			set.URLs = append(set.URLs, url{
				Loc:     p.baseURL + "/" + strings.TrimPrefix(*node.Address, "/"),
				LastMod: w3cDate(node.UpdatedAt),
			})
		}
	}
	body, err := xml.MarshalIndent(set, "", "  ")
	if err != nil {
		ctx.Fail(&web.Error{Status: http.StatusInternalServerError, Message: "生成 sitemap 失败"})
		return
	}
	ctx.SetHeader("Content-Type", "application/xml; charset=utf-8")
	ctx.SetHeader("Cache-Control", "no-cache")
	_, _ = ctx.W.Write([]byte(xml.Header))
	_, _ = ctx.W.Write(body)
}

// collect 分页读一个类型的**可读**节点（读规则、掩码、行范围全部由读入口保证）。
func (p *Plugin) collect(ctx *web.CmsCtx, typeName string) ([]*core.Node, error) {
	out := []*core.Node{}
	for offset := 0; ; offset += pageSize {
		nodes, _, err := ctx.List(core.NodeQuery{Type: typeName}, pageSize, offset)
		if err != nil {
			return nil, err
		}
		out = append(out, nodes...)
		if len(nodes) < pageSize {
			return out, nil
		}
	}
}

// w3cDate Unix 秒 → W3C 日期（sitemap 的 lastmod 格式）。
func w3cDate(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}
