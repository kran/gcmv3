// Package settings 站点配置插件（key-value，**库是真相**）。
//
// 一行一条配置:
//
//	key        配置键（字母/数字/下划线/连字符/点）
//	group_name 分组（后台按它筛; 空 = 未分组）
//	type       编辑形态（type 列 —— 后台拿它选控件; **服务端不校验**, 见下）
//	value      JSON 值
//	updated_at Unix 秒（跟全站一致）
//
// 后台就是 v2 那套: 表格 + 分组筛选 + 弹窗增删改, **能建任意键**; 类型与值都不校验
// （v2 的 SetSetting 也只查键格式 + JSON 编码）—— 类型只是"用哪个控件编这个值"的提示。
//
// Options.Items 是**可选的预置项**: Label/Note/Options/Default/Group 只影响后台显示与
// "没设过时的默认值", 不拦写入（没预置的键照样能建）。
//
// 站点侧读法:
//
//	sp, err := settings.Mount(site, settings.Options{Items: []settings.Item{
//	    {Key: "site.phone", Kind: settings.KindText, Label: "联系电话", Default: "020-0000"},
//	}})
//	phone := sp.String("site.phone")   // 没设过 ⇒ 预置的 Default
//
// 已知取舍: 键是自由字符串 ⇒ 读一个"既没预置也没写过"的键是**静默空值**（v2 一样）。
// 要严格就用 Get（返回 false）或 All() 对账 —— 自由 KV 与"拼错键名当场报错"只能选一个。
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kran/cho"
	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/web"
)

// DefaultPrefix 管理端点前缀（Options.Prefix 可改）。
const DefaultPrefix = "/settings"

// 支持的编辑形态 —— **纯粹是后台选控件的提示**, 服务端不校验（v2 就是这样:
// SetSetting 只查键格式 + JSON 编码, type 是原样存下来的字符串）。
//
// 库里存了别的名字也能存能读 —— 只是后台找不到对应控件。
const (
	KindText        = "text"
	KindTextarea    = "textarea"
	KindRichtext    = "richtext" // 富文本（值就是 HTML）
	KindNumber      = "number"
	KindBool        = "bool"
	KindSelect      = "select" // 枚举（候选值来自预置声明, 或后台自己写）
	KindUploadImage = "upload-image"
	KindUploadFile  = "upload-file"
	KindJSON        = "json"   // 任意 JSON
	KindObject      = "object" // 键值对（后台给结构化的编辑器, 存下来还是 JSON 对象）
	KindArray       = "array"  // 值列表
)

// ErrNotFound 删一条不存在的配置。
var ErrNotFound = errors.New("settings: 没有这一条配置")

// Item 一条配置的**预置声明**（可选 —— 只是后台的标签/默认值/候选项）。
type Item struct {
	Key     string   // "site.phone"
	Group   string   // 后台分组（空 = 未分组）
	Kind    string   // 后台新建这一行时的默认形态（提示而已, 见上面那组常量）
	Label   string   // 后台显示名
	Note    string   // 后台提示（可选）
	Options []string // Kind=select 时的候选值（后台下拉用）
	Default any      // 没设过时的值（后台"删除"= 回到它）
}

// Options 插件配置。
type Options struct {
	// Items 预置清单（键唯一、顺序即后台显示顺序）。**可以为空** —— 空就是纯自由 KV。
	Items []Item
	// Prefix 管理端点前缀（空 = DefaultPrefix）。
	Prefix string
	// Roles 除了 owner 之外, 还有哪些角色能在后台改（空 = 仅 owner）。
	// 站点配置通常是"能改站点门面"的权限, 比内容管理更敏感。
	Roles []string
}

// Plugin 装好的配置插件（每个站点一个）。
type Plugin struct {
	site   *web.Site
	prefix string
	roles  []string
	items  []Item
	byKey  map[string]Item
}

// row 库里的一行（`value` 是 JSON 文本）。
type row struct {
	Key       string `db:"key"`
	GroupName string `db:"group_name"`
	Type      string `db:"type"`
	RawValue  string `db:"value"`
	UpdatedAt int64  `db:"updated_at"`
}

// Mount 装插件: 建表（含老表补列）+ 挂后台端点与面板。
func Mount(site *web.Site, options Options) (*Plugin, error) {
	if site == nil {
		return nil, errors.New("settings: site is required")
	}
	plugin := &Plugin{
		site:   site,
		prefix: strings.TrimSpace(options.Prefix),
		roles:  options.Roles,
		byKey:  map[string]Item{},
	}
	if plugin.prefix == "" {
		plugin.prefix = DefaultPrefix
	}
	for _, item := range options.Items {
		item.Key = strings.TrimSpace(item.Key)
		err := plugin.checkItem(item)
		if err != nil {
			return nil, err
		}
		_, dup := plugin.byKey[item.Key]
		if dup {
			return nil, fmt.Errorf("settings: 键 %q 预置了两次", item.Key)
		}
		plugin.byKey[item.Key] = item
		plugin.items = append(plugin.items, item)
	}

	err := plugin.createTable()
	if err != nil {
		return nil, err
	}
	err = plugin.mountRoutes()
	if err != nil {
		return nil, err
	}
	return plugin, nil
}

// checkItem 预置声明自身的校验（拼错就在这里爆，不留到后台点开才发现）。
//
// 只管**声明自己的完整性**: 键不合法、同一个键声明两次、声明了 select 却不给候选值。
// Kind 与 Default 不校验 —— 那是提示与默认值, 不是写入契约（服务端不拿它卡数据）。
func (p *Plugin) checkItem(item Item) error {
	if item.Key == "" {
		return errors.New("settings: 配置键不能为空")
	}
	if !validKey(item.Key) {
		return fmt.Errorf("settings: 键 %q 只能由字母/数字/下划线/连字符/点组成", item.Key)
	}
	if item.Kind == "" {
		return fmt.Errorf("settings: %q 没写 Kind（后台新建这一行时不知道该用什么控件）", item.Key)
	}
	if item.Kind == KindSelect && len(item.Options) == 0 {
		return fmt.Errorf("settings: %q 是 select 但没给 Options（后台只能给个手填框）", item.Key)
	}
	return nil
}

// validKey 键格式: 字母/数字/下划线/连字符/点（与 v2 的 checkKey 一模一样 ——
// 服务端唯一要卡的就是这个, 值与类型都是自由的）。
func validKey(key string) bool {
	if key == "" {
		return false
	}
	for _, char := range key {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9', char == '_', char == '-', char == '.':
		default:
			return false
		}
	}
	return true
}

// createTable 建表 + 老表补列。
//
// v3 初版的表只有 key/value/updated_at（声明驱动那版）—— 老库要能直接用: SQLite 的
// ALTER TABLE ADD COLUMN 带常量默认值不重写表, 数据一个不丢。补出来的 type/group 只是
// **占位**, 紧接着用预置声明回填一遍（否则一条 richtext 配置会变成 text, 后台就按错
// 的控件渲染）。
func (p *Plugin) createTable() error {
	_, err := p.site.DB().Add(`CREATE TABLE IF NOT EXISTS settings (
		key        TEXT PRIMARY KEY,
		group_name TEXT NOT NULL DEFAULT '',
		type       TEXT NOT NULL DEFAULT 'text',
		value      TEXT NOT NULL DEFAULT '',
		updated_at INTEGER NOT NULL DEFAULT 0
	)`).Exec()
	if err != nil {
		return fmt.Errorf("settings: 建表: %w", err)
	}
	columns, err := p.columns()
	if err != nil {
		return err
	}
	addedType := !columns["type"]
	if !columns["group_name"] {
		_, err = p.site.DB().Add(`ALTER TABLE settings ADD COLUMN group_name TEXT NOT NULL DEFAULT ''`).Exec()
		if err != nil {
			return fmt.Errorf("settings: 补 group_name 列: %w", err)
		}
	}
	if addedType {
		_, err = p.site.DB().Add(`ALTER TABLE settings ADD COLUMN type TEXT NOT NULL DEFAULT 'text'`).Exec()
		if err != nil {
			return fmt.Errorf("settings: 补 type 列: %w", err)
		}
	}
	if addedType {
		err = p.backfillPresets()
		if err != nil {
			return err
		}
	}
	return nil
}

// backfillPresets 用预置声明回填刚补出来的 group_name / type。
func (p *Plugin) backfillPresets() error {
	for _, item := range p.items {
		// 只补"还是占位值"的行: 后台后来改过的类型不能被启动时又改回去
		_, err := p.site.DB().Add(`UPDATE settings SET type = #{1}, group_name = #{2}
			WHERE key = #{3} AND type = #{4}`,
			item.Kind, item.Group, item.Key, KindText).Exec()
		if err != nil {
			return fmt.Errorf("settings: 回填 %q: %w", item.Key, err)
		}
	}
	return nil
}

// columnInfo PRAGMA table_info 的一行（sqlx 的结构映射要**每个列都有落点** —— PRAGMA
// 固定回这 6 列, 少一个都会 "missing destination name"）。
type columnInfo struct {
	CID      int     `db:"cid"`
	Name     string  `db:"name"`
	Type     string  `db:"type"`
	NotNull  int     `db:"notnull"`
	Default  *string `db:"dflt_value"` // 没默认值就是 NULL
	PrimaryK int     `db:"pk"`
}

// columns 现有列名集合。
func (p *Plugin) columns() (map[string]bool, error) {
	rows, err := p.site.DB().Add(`PRAGMA table_info(settings)`).FetchList[columnInfo]()
	if err != nil {
		return nil, fmt.Errorf("settings: 读表结构: %w", err)
	}
	out := make(map[string]bool, len(rows))
	for _, one := range rows {
		out[one.Name] = true
	}
	return out, nil
}

func (p *Plugin) mountRoutes() error {
	p.site.Hook(web.HookAdminMount, func(g *cho.Cho[*web.CmsCtx]) error {
		g.Get(p.prefix, p.list)
		g.Post(p.prefix, p.upsert)
		g.Delete(p.prefix+"/{key}", p.remove)
		g.Get(p.prefix+"/panel.vue", p.panel)
		return nil
	})
	p.site.Hook(web.HookAdminPanel, func(_ *web.CmsCtx, panels *core.List[web.AdminPanel]) error {
		panels.Append(web.AdminPanel{
			Path: p.prefix, Title: "站点配置", Vue: "/admin" + p.prefix + "/panel.vue",
		})
		return nil
	})
	return nil
}

// ── 站点侧读接口（服务端代码用）────────────────────────────────────────────

// Get 取一条并解码到 dest。
//
//	(true, nil)   有值（库里的, 或预置的 Default）
//	(false, nil)  没有（既没写过也没默认值）—— 自由 KV 里这就是正常状态
//	(false, err)  值解不进 dest（形态不匹配 —— 这个要响）
func (p *Plugin) Get(key string, dest any) (bool, error) {
	value, ok, err := p.raw(key)
	if err != nil {
		return false, err
	}
	if !ok {
		item, declared := p.byKey[key]
		if !declared || item.Default == nil {
			return false, nil
		}
		value = item.Default
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	err = json.Unmarshal(encoded, dest)
	if err != nil {
		return false, fmt.Errorf("settings: %q 的值解不进目标类型: %w", key, err)
	}
	return true, nil
}

// String 取字符串配置（没设过 ⇒ 预置的 Default，取不到就是空串）。
func (p *Plugin) String(key string) string {
	var out string
	_, _ = p.Get(key, &out)
	return out
}

// Int 取数字配置（没设过/类型不符 ⇒ 0）。
func (p *Plugin) Int(key string) int64 {
	var out int64
	_, _ = p.Get(key, &out)
	return out
}

// Bool 取布尔配置。
func (p *Plugin) Bool(key string) bool {
	var out bool
	_, _ = p.Get(key, &out)
	return out
}

// All 全部配置（预置项 + 库里的行）—— 站点拼 /api/home 这类响应时用它。
func (p *Plugin) All() (map[string]any, error) {
	rows, err := p.loadRows()
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(p.items)+len(rows))
	for _, item := range p.items {
		out[item.Key] = item.Default
	}
	for _, one := range rows {
		var value any
		err = json.Unmarshal([]byte(one.RawValue), &value)
		if err != nil {
			return nil, fmt.Errorf("settings: %q 的值不是合法 JSON: %w", one.Key, err)
		}
		out[one.Key] = value
	}
	return out, nil
}

// Set 写一条（upsert: 有就改, 没有就建 —— v2 的语义, 键不必预置）。
//
// **只管键格式, 不校验类型/值**（v2 就这么干的）: type 是后台选控件的提示, value 原样
// JSON 编码存下来。把文本填进 number 行是运营的事 —— 站点读到什么就是什么, 服务端不替
// 它猜, 也不拦。
func (p *Plugin) Set(key, group, typ string, value any) error {
	key = strings.TrimSpace(key)
	if !validKey(key) {
		return fmt.Errorf("settings: 键 %q 只能由字母/数字/下划线/连字符/点组成", key)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("settings: %q 的值编不成 JSON: %w", key, err)
	}
	_, err = p.site.DB().Add(`INSERT INTO settings (key, group_name, type, value, updated_at)
		VALUES (#{1}, #{2}, #{3}, #{4}, #{5})
		ON CONFLICT(key) DO UPDATE SET group_name = #{2}, type = #{3}, value = #{4}, updated_at = #{5}`,
		key, strings.TrimSpace(group), strings.TrimSpace(typ), string(encoded), time.Now().Unix()).Exec()
	if err != nil {
		return fmt.Errorf("settings: 写 %q: %w", key, err)
	}
	return nil
}

// Delete 删一条（回到预置声明的 Default; 没这一行 ⇒ 报错）。
func (p *Plugin) Delete(key string) error {
	affected, err := p.site.DB().Add(`DELETE FROM settings WHERE key = #{1}`, key).Exec()
	if err != nil {
		return fmt.Errorf("settings: 删 %q: %w", key, err)
	}
	n, err := affected.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, key)
	}
	return nil
}

// raw 取库里的原始值（没写过 ⇒ false）。
func (p *Plugin) raw(key string) (any, bool, error) {
	one, err := p.site.DB().Add(`SELECT * FROM settings WHERE key = #{1}`, key).FetchOne[row]()
	if err != nil {
		return nil, false, err
	}
	if one == nil {
		return nil, false, nil
	}
	var value any
	err = json.Unmarshal([]byte(one.RawValue), &value)
	if err != nil {
		return nil, false, fmt.Errorf("settings: %q 的值不是合法 JSON: %w", key, err)
	}
	return value, true, nil
}

// loadRows 全表（按 key 排序）。
func (p *Plugin) loadRows() ([]row, error) {
	rows, err := p.site.DB().Add(`SELECT * FROM settings ORDER BY key`).FetchList[row]()
	if err != nil {
		return nil, fmt.Errorf("settings: 读配置: %w", err)
	}
	return rows, nil
}

// ── 后台端点 ───────────────────────────────────────────────────────────────

// settingItem 后台看到的一条: 预置声明（有的话）+ 库里的当前状态。
type settingItem struct {
	Key       string   `json:"key"`
	Group     string   `json:"group"`
	Type      string   `json:"type"`
	Label     string   `json:"label,omitempty"`
	Note      string   `json:"note,omitempty"`
	Options   []string `json:"options,omitempty"`
	Default   any      `json:"default,omitempty"`
	Value     any      `json:"value"`
	UpdatedAt int64    `json:"updated_at"` // 0 = 没写过（值来自 Default 或空）
}

// list GET {prefix} —— 全表 + 预置项（面板自己按分组筛 —— 服务端筛过的话, 分组下拉
// 里的其它分组会跟着消失, v2 就是这么坏的）。
func (p *Plugin) list(ctx *web.CmsCtx) {
	if !p.allowed(ctx) {
		return
	}
	rows, err := p.loadRows()
	if err != nil {
		ctx.Fail(err)
		return
	}
	stored := make(map[string]row, len(rows))
	for _, one := range rows {
		stored[one.Key] = one
	}
	items := make([]settingItem, 0, len(p.items)+len(rows))
	for _, item := range p.items {
		entry := settingItem{
			Key: item.Key, Group: item.Group, Type: item.Kind, Label: item.Label,
			Note: item.Note, Options: item.Options, Default: item.Default, Value: item.Default,
		}
		one, ok := stored[item.Key]
		if ok {
			entry.Group, entry.Type = one.GroupName, one.Type
			entry.Value = decodeValue(entry.Key, one.RawValue)
			entry.UpdatedAt = one.UpdatedAt
			delete(stored, item.Key)
		}
		items = append(items, entry)
	}
	rest := make([]string, 0, len(stored))
	for key := range stored {
		rest = append(rest, key)
	}
	sort.Strings(rest)
	for _, key := range rest {
		one := stored[key]
		items = append(items, settingItem{
			Key: key, Group: one.GroupName, Type: one.Type,
			Value: decodeValue(key, one.RawValue), UpdatedAt: one.UpdatedAt,
		})
	}
	_ = ctx.Json(http.StatusOK, map[string]any{"items": items})
}

// decodeValue 库里存的 JSON 文本 → 值（坏 JSON 当原样字符串 —— 后台得能看见并改掉它,
// 报错只会让人打不开这个页面）。
func decodeValue(key, raw string) any {
	var value any
	err := json.Unmarshal([]byte(raw), &value)
	if err != nil {
		return raw
	}
	return value
}

// setInput POST {prefix} 的请求体。
type setInput struct {
	Key   string `json:"key"`
	Group string `json:"group"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// upsert POST {prefix} —— 新建或改写一条（v2 的保存方式: 键写在 body 里）。
func (p *Plugin) upsert(ctx *web.CmsCtx) {
	if !p.allowed(ctx) {
		return
	}
	var input setInput
	err := ctx.BindJSON(&input)
	if err != nil {
		ctx.Fail(err)
		return
	}
	err = p.Set(input.Key, input.Group, input.Type, input.Value)
	if err != nil {
		ctx.Fail(SettingError(err))
		return
	}
	_ = ctx.NoContent(http.StatusNoContent)
}

// remove DELETE {prefix}/{key} —— 删一条。
func (p *Plugin) remove(ctx *web.CmsCtx) {
	if !p.allowed(ctx) {
		return
	}
	key := strings.TrimSpace(ctx.PathValue("key"))
	err := p.Delete(key)
	if err != nil {
		ctx.Fail(SettingError(err))
		return
	}
	_ = ctx.NoContent(http.StatusNoContent)
}

// panel 面板组件源码（embed — 后台无构建, 运行时编译）。
//
// __PREFIX__ 换成实际前缀后发出去: 面板里的请求地址得跟着 Options.Prefix 走, 不然把
// 前缀配成别的值时后台就打到默认地址上（404）。
func (p *Plugin) panel(ctx *web.CmsCtx) {
	data, err := panelFS.ReadFile("web/settings.vue")
	if err != nil {
		ctx.Fail(&web.Error{Status: http.StatusInternalServerError, Message: "面板资源缺失"})
		return
	}
	body := strings.ReplaceAll(string(data), "__PREFIX__", "/admin"+p.prefix)
	ctx.SetHeader("Content-Type", "text/javascript; charset=utf-8")
	ctx.SetHeader("Cache-Control", "no-cache")
	_, _ = ctx.W.Write([]byte(body))
}

// SettingError 把插件错误翻译成 HTTP（键/值不合法 = 400, 没这一条 = 404）。
func SettingError(err error) *web.Error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return web.NotFound("%s", err.Error())
	default:
		return web.BadRequest("%s", err.Error())
	}
}

// allowed 谁能在后台改: owner, 或 Options.Roles 里列出的角色。
func (p *Plugin) allowed(ctx *web.CmsCtx) bool {
	actor := ctx.Actor()
	if actor.IsOwner() {
		return true
	}
	for _, role := range p.roles {
		if role != "" && actor.HasRole(role) {
			return true
		}
	}
	ctx.Fail(web.Forbidden("只有 owner 能改站点配置"))
	return false
}
