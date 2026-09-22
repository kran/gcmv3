// Package settings 站点配置插件（key-value，**声明驱动**）。
//
// 与 v2 那套（core/settings.go）的差别，也是这次重写的取舍：
//
//   - **没有"分组"**。配置项就几十条，分组只会多一层没人约束的字符串
//     （迟早出现 site / siteinfo / 站点 三种写法）。后台按声明顺序平铺。
//   - **存储里没有 group/type 列**。类型是**声明**（Options.Items），不是数据 ——
//     存进库里就会和代码漂移：改了声明忘了改库，后台就按错的形态渲染，
//     而且没人会想起来去看那一列。表里只留 key + value + updated_at。
//   - **只能改已声明的键**。写入未声明的 key 直接拒（fail-loud）：配置是代码的事，
//     不是数据的事 —— 从后台凭空多出来的键没有类型、没有说明，谁也不知道该长什么样。
//   - 时间用 **Unix 秒**（跟全站一致），不再是 v2 的 ISO 字符串。
//
// 站点怎么用：
//
//	sp, err := settings.Mount(site, settings.Options{Items: []settings.Item{
//	    {Key: "site.phone", Kind: "text", Label: "联系电话"},
//	    {Key: "site.logo",  Kind: "upload-image", Label: "站点 Logo"},
//	}})
//	phone := sp.String("site.phone")            // 未设时给声明里的 Default
//
// 后台面板 / 端点由插件自己挂（AdminMount + AdminPanel）。
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kran/cho"
	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/types"
	"github.com/kran/gcmv3/web"
)

// DefaultPrefix 管理端点前缀（Options.Prefix 可改）。
const DefaultPrefix = "/settings"

// 支持的编辑形态 —— 与后台的 widget kind 同名（面板据此渲染控件）。
const (
	KindText        = "text"
	KindTextarea    = "textarea"
	KindNumber      = "number"
	KindBool        = "bool"
	KindSelect      = "select"
	KindUploadImage = "upload-image"
	KindUploadFile  = "upload-file"
	KindJSON        = "json" // 任意 JSON（给结构化配置用）
)

// ErrUndeclared 写了一个没声明的键。
var ErrUndeclared = errors.New("settings: undeclared key")

// Item 一条配置的**声明**（类型、标签、默认值都在这里 —— 唯一真相）。
type Item struct {
	Key   string // "site.phone"（字母/数字/下划线/连字符/点）
	Kind  string // 见上面那组常量
	Label string // 后台显示名
	Note  string // 后台提示（可选）
	// Options Kind=select 时的候选值。
	Options []string
	// Default 未设置时的值（后台"清空"= 回到它）。
	Default any
}

// Options 插件配置。
type Options struct {
	// Items 声明清单（键唯一、顺序即后台显示顺序）。
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

type row struct {
	Key       string `db:"key"`
	RawValue  string `db:"value"`
	UpdatedAt int64  `db:"updated_at"`
}

// Mount 装插件: 建表 + 挂后台端点与面板。
func Mount(site *web.Site, options Options) (*Plugin, error) {
	if site == nil {
		return nil, errors.New("settings: site is required")
	}
	if len(options.Items) == 0 {
		return nil, errors.New("settings: 至少声明一条配置（装了空清单没意义）")
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
		if _, dup := plugin.byKey[item.Key]; dup {
			return nil, fmt.Errorf("settings: 键 %q 声明了两次", item.Key)
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

// checkItem 声明自身的校验（拼错就在这里爆，不留到后台点开才发现）。
func (p *Plugin) checkItem(item Item) error {
	if item.Key == "" {
		return errors.New("settings: 配置键不能为空")
	}
	if !validKey(item.Key) {
		return fmt.Errorf("settings: 键 %q 只能由字母/数字/下划线/连字符/点组成", item.Key)
	}
	switch item.Kind {
	case KindText, KindTextarea, KindNumber, KindBool, KindSelect,
		KindUploadImage, KindUploadFile, KindJSON:
	case "":
		return fmt.Errorf("settings: %q 没写 Kind（后台不知道该用什么控件）", item.Key)
	default:
		return fmt.Errorf("settings: %q 的 Kind %q 不认识（可选: %s）", item.Key, item.Kind, strings.Join(Kinds(), ", "))
	}
	if item.Kind == KindSelect && len(item.Options) == 0 {
		return fmt.Errorf("settings: %q 是 select 但没给 Options", item.Key)
	}
	if item.Default != nil {
		_, err := normalize(item, item.Default)
		if err != nil {
			return fmt.Errorf("settings: %q 的 Default 不合法: %w", item.Key, err)
		}
	}
	return nil
}

// Kinds 支持的编辑形态（给报错信息和文档用）。
func Kinds() []string {
	return []string{KindText, KindTextarea, KindNumber, KindBool, KindSelect,
		KindUploadImage, KindUploadFile, KindJSON}
}

// validKey 键格式: 字母/数字/下划线/连字符/点（与 v2 一致）。
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

func (p *Plugin) createTable() error {
	_, err := p.site.DB().Add(`CREATE TABLE IF NOT EXISTS settings (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	)`).Exec()
	if err != nil {
		return fmt.Errorf("settings: 建表: %w", err)
	}
	return nil
}

func (p *Plugin) mountRoutes() error {
	p.site.Hook(web.HookAdminMount, func(g *cho.Cho[*web.CmsCtx]) error {
		g.Get(p.prefix, p.list)
		g.Put(p.prefix+"/{key}", p.set)
		g.Delete(p.prefix+"/{key}", p.clear)
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

// Get 取一条并解码到 dest（未设置 ⇒ false, 不报错）。
func (p *Plugin) Get(key string, dest any) (bool, error) {
	item, ok := p.byKey[key]
	if !ok {
		return false, fmt.Errorf("%w: %q", ErrUndeclared, key)
	}
	value, ok, err := p.raw(key)
	if err != nil {
		return false, err
	}
	if !ok {
		value = item.Default
	}
	if value == nil {
		return false, nil
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

// String 取字符串配置（未设置 ⇒ 声明里的 Default，取不到就是空串）。
func (p *Plugin) String(key string) string {
	var out string
	_, _ = p.Get(key, &out)
	return out
}

// Int 取数字配置（未设置/类型不符 ⇒ 0）。
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

// All 声明过的键 → 当前值（含默认值）。站点拼 /api/home 这类响应时用它。
func (p *Plugin) All() (map[string]any, error) {
	out := make(map[string]any, len(p.items))
	stored, err := p.loadAll()
	if err != nil {
		return nil, err
	}
	for _, item := range p.items {
		if raw, ok := stored[item.Key]; ok {
			out[item.Key] = raw
			continue
		}
		out[item.Key] = item.Default
	}
	return out, nil
}

// Set 写一条（键必须已声明, 值必须符合声明的形态）。
func (p *Plugin) Set(key string, value any) error {
	item, ok := p.byKey[key]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUndeclared, key)
	}
	normalized, err := normalize(item, value)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	_, err = p.site.DB().Add(`INSERT INTO settings (key, value, updated_at) VALUES (#{1}, #{2}, #{3})
		ON CONFLICT(key) DO UPDATE SET value = #{2}, updated_at = #{3}`,
		item.Key, string(encoded), time.Now().Unix()).Exec()
	if err != nil {
		return fmt.Errorf("settings: 写 %q: %w", key, err)
	}
	return nil
}

// Clear 清空一条（回到声明里的 Default）。
func (p *Plugin) Clear(key string) error {
	if _, ok := p.byKey[key]; !ok {
		return fmt.Errorf("%w: %q", ErrUndeclared, key)
	}
	_, err := p.site.DB().Add(`DELETE FROM settings WHERE key = #{1}`, key).Exec()
	if err != nil {
		return fmt.Errorf("settings: 清 %q: %w", key, err)
	}
	return nil
}

// raw 取库里的原始值（未设置 ⇒ false）。
func (p *Plugin) raw(key string) (any, bool, error) {
	row, err := p.site.DB().Add(`SELECT * FROM settings WHERE key = #{1}`, key).FetchOne[row]()
	if err != nil {
		return nil, false, err
	}
	if row == nil {
		return nil, false, nil
	}
	var value any
	err = json.Unmarshal([]byte(row.RawValue), &value)
	if err != nil {
		return nil, false, fmt.Errorf("settings: %q 的值不是合法 JSON: %w", key, err)
	}
	return value, true, nil
}

// loadAll 一次取全表（键 → 值）。
func (p *Plugin) loadAll() (map[string]any, error) {
	rows, err := p.site.DB().Add(`SELECT * FROM settings`).FetchList[row]()
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(rows))
	for _, item := range rows {
		var value any
		err = json.Unmarshal([]byte(item.RawValue), &value)
		if err != nil {
			return nil, fmt.Errorf("settings: %q 的值不是合法 JSON: %w", item.Key, err)
		}
		out[item.Key] = value
	}
	return out, nil
}

// normalize 按声明的形态校验并归一值。
//
// 类型不符一律拒（fail-loud）: 一个 number 配置存进 "abc", 站点读的时候要么崩要么静默
// 拿 0 —— 不如在写入口就说清楚。
func normalize(item Item, value any) (any, error) {
	switch item.Kind {
	case KindBool:
		boolean, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("settings: %q 要布尔值, 收到 %T", item.Key, value)
		}
		return boolean, nil
	case KindNumber:
		number, err := types.ToID(value)
		if err != nil {
			return nil, fmt.Errorf("settings: %q 要数字, 收到 %T", item.Key, value)
		}
		return number, nil
	case KindSelect:
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("settings: %q 要字符串, 收到 %T", item.Key, value)
		}
		if slices.Contains(item.Options, text) {
			return text, nil
		}
		return nil, fmt.Errorf("settings: %q 只能是 %s 之一, 收到 %q",
			item.Key, strings.Join(item.Options, " / "), text)
	case KindJSON:
		return value, nil
	default: // text / textarea / upload-image / upload-file
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("settings: %q 要字符串, 收到 %T", item.Key, value)
		}
		return text, nil
	}
}

// ── 后台端点 ───────────────────────────────────────────────────────────────

// settingItem 后台看到的一条: 声明 + 当前值。
type settingItem struct {
	Key       string   `json:"key"`
	Kind      string   `json:"kind"`
	Label     string   `json:"label"`
	Note      string   `json:"note,omitempty"`
	Options   []string `json:"options,omitempty"`
	Default   any      `json:"default,omitempty"`
	Value     any      `json:"value"`
	UpdatedAt int64    `json:"updated_at"` // 0 = 还是默认值
}

// list GET {prefix} —— 声明顺序的清单 + 当前值（面板据此渲染）。
func (p *Plugin) list(ctx *web.CmsCtx) {
	if !p.allowed(ctx) {
		return
	}
	stored, err := p.loadTimes()
	if err != nil {
		ctx.Fail(err)
		return
	}
	items := make([]settingItem, 0, len(p.items))
	for _, item := range p.items {
		value, ok, err := p.raw(item.Key)
		if err != nil {
			ctx.Fail(err)
			return
		}
		if !ok {
			value = item.Default
		}
		items = append(items, settingItem{
			Key: item.Key, Kind: item.Kind, Label: item.Label, Note: item.Note,
			Options: item.Options, Default: item.Default,
			Value: value, UpdatedAt: stored[item.Key],
		})
	}
	_ = ctx.Json(http.StatusOK, map[string]any{"items": items})
}

// loadTimes 键 → 更新时间（0 = 还是默认值）。
func (p *Plugin) loadTimes() (map[string]int64, error) {
	rows, err := p.site.DB().Add(`SELECT * FROM settings`).FetchList[row]()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, item := range rows {
		out[item.Key] = item.UpdatedAt
	}
	return out, nil
}

type setInput struct {
	Value any `json:"value"`
}

// set PUT {prefix}/{key} —— 写一条。
func (p *Plugin) set(ctx *web.CmsCtx) {
	if !p.allowed(ctx) {
		return
	}
	key := strings.TrimSpace(ctx.PathValue("key"))
	var input setInput
	err := ctx.BindJSON(&input)
	if err != nil {
		ctx.Fail(err)
		return
	}
	err = p.Set(key, input.Value)
	if err != nil {
		ctx.Fail(SettingError(err))
		return
	}
	_ = ctx.NoContent(http.StatusNoContent)
}

// clear DELETE {prefix}/{key} —— 清空（回到默认）。
func (p *Plugin) clear(ctx *web.CmsCtx) {
	if !p.allowed(ctx) {
		return
	}
	key := strings.TrimSpace(ctx.PathValue("key"))
	err := p.Clear(key)
	if err != nil {
		ctx.Fail(SettingError(err))
		return
	}
	_ = ctx.NoContent(http.StatusNoContent)
}

// panel 面板组件源码（embed — 后台无构建, 运行时编译）。
func (p *Plugin) panel(ctx *web.CmsCtx) {
	data, err := panelFS.ReadFile("web/settings.vue")
	if err != nil {
		ctx.Fail(&web.Error{Status: http.StatusInternalServerError, Message: "面板资源缺失"})
		return
	}
	ctx.SetHeader("Content-Type", "text/javascript; charset=utf-8")
	ctx.SetHeader("Cache-Control", "no-cache")
	_, _ = ctx.W.Write(data)
}

// SettingError 把插件错误翻译成 HTTP（未声明的键 = 404, 值不合法 = 400）。
func SettingError(err error) *web.Error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUndeclared):
		return web.NotFound("没有这个配置项（配置项由站点代码声明, 后台只能改值）")
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

// Keys 声明过的键（按声明顺序）—— 给站点的诊断/文档用。
func (p *Plugin) Keys() []string {
	out := make([]string, 0, len(p.items))
	for _, item := range p.items {
		out = append(out, item.Key)
	}
	return out
}

// SortedKeys 排序后的键（日志/对比用）。
func (p *Plugin) SortedKeys() []string {
	out := p.Keys()
	sort.Strings(out)
	return out
}
