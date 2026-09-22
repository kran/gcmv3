package web

import (
	"fmt"
	"net/http"
	"strings"
)

// HostMux 多站分发: 一个进程按 Host 头服务多个 Site。
//
// 为什么需要它: 站点多起来之后"一站一进程"意味着 n 份 systemd / n 个端口 / n 份日志
// 轮转。分发器只是个小 helper —— 站点之间本来就没有共享状态（各自一个库文件、一个
// Site、一份路由）:
//
//	mux := web.NewHostMux()
//	a, err := mux.Add([]string{"a.com", "www.a.com"}, "sites/a") // 配置期照单站那样 Hook
//	...
//	http.ListenAndServe(":8080", mux.Start())                   // Start 把每个站 Setup 一遍
//
// 与"一站一进程"不冲突: 想拆随时拆 —— 把 Add 那几行挪进各自的 main, 别的都不用改。
// 配置期就是普通 Site（Hook / 插件 Mount / 策略 / 模板函数都照旧）。
//
// Host 匹配: 小写、**去掉端口**（"a.com:8080" 与 "a.com" 同一条）; 未匹配 ⇒
// SetFallback 设的兜底站点, 没设就 404。
type HostMux struct {
	routes   map[string]*Site // host(小写, 无端口) → site
	fallback *Site
}

// NewHostMux 建分发器。
func NewHostMux() *HostMux {
	return &HostMux{routes: map[string]*Site{}}
}

// Add 建一个站点（Open）并注册它认的 Host。返回 *Site 供配置期使用。
//
// 两处 fail-loud: 目录打不开（Open 的错, 直接透出）、Host 被别的站点占了
// （v2 是静默覆盖 —— 两个站抢同一个域名, 谁能访问全看 map 的写入顺序, 最难查）。
func (m *HostMux) Add(hosts []string, basedir string) (*Site, error) {
	site, err := Open(basedir)
	if err != nil {
		return nil, err
	}
	added := 0
	for _, host := range hosts {
		key := normalizeHost(host)
		if key == "" {
			continue
		}
		if existing, ok := m.routes[key]; ok && existing != site {
			return nil, fmt.Errorf("web: Host %q 已经被别的站点占了（同一个域名只能属于一个站）", key)
		}
		m.routes[key] = site
		added++
	}
	if added == 0 {
		return nil, fmt.Errorf("web: %s 没给可用的 Host（Add 的 hosts 为空）", basedir)
	}
	return site, nil
}

// SetFallback 兜底站点（未匹配 Host 时用; 可选）。
func (m *HostMux) SetFallback(site *Site) { m.fallback = site }

// Start 把每个站点 Setup 一遍（挂路由, 幂等）并返回 mux 自己（http.Handler）。
func (m *HostMux) Start() http.Handler {
	for _, site := range m.sites() {
		site.Setup()
	}
	return m
}

// ServeHTTP 按 Host 分发。
func (m *HostMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	site := m.routes[normalizeHost(r.Host)]
	if site == nil {
		site = m.fallback
	}
	if site == nil {
		http.NotFound(w, r)
		return
	}
	site.Setup().ServeHTTP(w, r)
}

// sites 去重后的站点清单（同一个 Site 注册了多个 Host 只算一次）。
func (m *HostMux) sites() []*Site {
	seen := map[*Site]bool{}
	out := make([]*Site, 0, len(m.routes))
	for _, site := range m.routes {
		if seen[site] {
			continue
		}
		seen[site] = true
		out = append(out, site)
	}
	if m.fallback != nil && !seen[m.fallback] {
		out = append(out, m.fallback)
	}
	return out
}

// normalizeHost 归一 Host: 去端口 + 小写（Host 头大小写不敏感）。
func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if index := strings.IndexByte(host, ':'); index >= 0 {
		host = host[:index]
	}
	return strings.ToLower(host)
}
