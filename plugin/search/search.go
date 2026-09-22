// Package search 全文检索插件（SQLite FTS5 + bigram 预分词）。
//
// 为什么是插件而不是 core:
//
//	检索不是每个站点都要, 也不是内核语义（它不改变数据的含义）—— v3 把 search
//	从 core 拿掉, 这里按"站点需要才装"的形态做回来。
//
// 四条设计:
//
//  1. **强一致的索引**: 订阅 core 的 HookNodeAfterCreate/Update/Delete（签名带着
//     `tx *dba.SQL`, 就在写事务里）⇒ 索引与节点同一个事务提交, 崩了不会漂。
//     想换外部引擎（bleve 之类）时, 忽略 tx 自己维护即可 —— 站点的代码不变。
//
//  2. **可搜索字段由类型声明决定**: 不维护第二份清单 —— 类型的字段声明里
//     `QueryOps.Text` 为真（text/textarea/richtext/address 这些 kind）就是可检索,
//     注释原文: "contains/prefix 和全文 searchable"。`Options.Types` 只说
//     "哪些类型参与检索"。
//
//  3. **结果走受管读入口回读**: 插件只负责"哪些 id 命中 + 相关度顺序", 节点本身用
//     ctx.List 回读 ⇒ 读规则（行范围）、字段掩码、展开一层**全部自动生效**。
//     插件不重复实现策略, 也不可能变成掩码/范围的旁路。
//
//  4. **游标翻页**: 游标 = (相关度, rowid)（FTS5 的 rank 可在 WHERE 里比较, 实测可用）。
//     翻页不会因为中途插入/删除而漂 —— 这是游标相对 page/offset 的真正价值
//     （性能上并不更省: 排序开销由命中规模决定, 每页都要重算）。
//
// 分词: CJK 段 bigram 滑窗（子串级精确、零依赖、新词自动覆盖）, 其余保留原词。
package search

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/kran/dba"
	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/web"
)

// DefaultPrefix 默认挂载路径（Options.Prefix 可改）。
const DefaultPrefix = "/api/search"

// DefaultSize 默认每页条数。
const DefaultSize = 20

// MaxSize 每页条数上限。
const MaxSize = 50

// DefaultMaxRunes 关键词长度上限（按字符数, 中文一个词也是 1）。
const DefaultMaxRunes = 100

// Options 插件配置。
type Options struct {
	// Types 参与检索的类型名（必须真实存在 —— 拼错在 Mount 当场报错）。
	//
	// 每个类型里"哪些字段进索引"由类型声明决定（QueryOps.Text）, 不在这里重复列。
	Types []string
	// Prefix 挂载路径（空 = DefaultPrefix）。
	Prefix string
	// MaxRunes 关键词长度上限（0 = DefaultMaxRunes）。
	MaxRunes int
}

// Plugin 装好的检索插件（每个站点一个）。
type Plugin struct {
	site           *web.Site
	prefix         string
	maxRunes       int
	searchableType map[string]bool
}

// Mount 装上插件: 建索引表 + 订阅节点写事件 + 挂检索路由。
//
// 幂等: 表用 IF NOT EXISTS; 路由重复注册会 panic（框架 fail-loud）—— 别装两次。
func Mount(site *web.Site, options Options) (*Plugin, error) {
	if site == nil {
		return nil, fmt.Errorf("search: site is required")
	}
	if len(options.Types) == 0 {
		return nil, fmt.Errorf("search: 至少声明一个可搜索类型（Options.Types 为空 —— 装了也没得搜）")
	}
	plugin := &Plugin{
		site:           site,
		prefix:         strings.TrimSpace(options.Prefix),
		maxRunes:       options.MaxRunes,
		searchableType: map[string]bool{},
	}
	if plugin.prefix == "" {
		plugin.prefix = DefaultPrefix
	}
	if plugin.maxRunes <= 0 {
		plugin.maxRunes = DefaultMaxRunes
	}
	for _, name := range options.Types {
		name = strings.TrimSpace(name)
		// 类型必须存在: 拼错就在这里爆（否则永远是"搜不到", 最难查）
		if _, ok := site.Types().Type(name); !ok {
			return nil, fmt.Errorf("search: 类型 %q 不存在（Options.Types 拼错了？）", name)
		}
		plugin.searchableType[name] = true
	}

	err := plugin.createTable()
	if err != nil {
		return nil, err
	}
	err = plugin.subscribe()
	if err != nil {
		return nil, err
	}
	err = plugin.mountRoute()
	if err != nil {
		return nil, err
	}
	// 索引是**空**的就建一次（老库第一次上 v3: 索引表刚建出来, 内容是 0 条）。
	//
	// 为什么让插件自己判断, 而不是让站点记"首次部署要重建": 站点很容易判错
	//（踩过: 用"有新迁移"当触发条件 ⇒ 老库索引一直是空的, 表现是"搜索永远没结果",
	// 而且不报错）。插件自己知道索引里有没有东西, 这是唯一不会判错的判据。
	// 非空则不动（已有索引的站点行为不变; 要全量重建仍用 Rebuild / -reindex）。
	err = plugin.rebuildIfEmpty()
	if err != nil {
		return nil, err
	}
	return plugin, nil
}

// rebuildIfEmpty 索引为空 ⇒ 全量建一次（幂等: 有内容就什么都不做）。
func (p *Plugin) rebuildIfEmpty() error {
	row, err := p.site.DB().Add(`SELECT COUNT(1) FROM search_fts`).FetchOne[int64]()
	if err != nil {
		return fmt.Errorf("search: 查索引条数: %w", err)
	}
	if row != nil && *row > 0 {
		return nil
	}
	slog.Info("search: 索引为空, 全量重建一次")
	return p.Rebuild()
}

// subscribe 把节点写事件接到索引维护上 —— 事件带 `tx *dba.SQL`, 就在写事务里,
// 所以索引与节点同生共死。
func (p *Plugin) subscribe() error {
	hooks := p.site.Engine().Hooks()
	events := []struct {
		name string
		fn   any
	}{
		{core.HookNodeAfterCreate, func(tx *dba.SQL, node *core.Node) error { return p.syncNode(tx, node) }},
		{core.HookNodeAfterUpdate, func(tx *dba.SQL, node *core.Node) error { return p.syncNode(tx, node) }},
		{core.HookNodeAfterDelete, func(tx *dba.SQL, nodeID int64) error { return p.deleteNode(tx, nodeID) }},
	}
	for _, event := range events {
		// AddHook 注册时就校验签名（写错类型当场报错, 不留到触发）
		err := hooks.AddHook(event.name, event.fn)
		if err != nil {
			return fmt.Errorf("search: 订阅 %s: %w", event.name, err)
		}
	}
	return nil
}

// mountRoute 挂检索路由（插件自己挂: 装了就一定能搜, 不用站点记得接线）。
func (p *Plugin) mountRoute() error {
	p.site.Hook(web.HookBeforeMount, func(s *web.Site) error {
		s.Router().Get(p.prefix, p.handle)
		return nil
	})
	return nil
}
