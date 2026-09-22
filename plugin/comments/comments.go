// Package comments 评论插件 —— 评论就是**一个节点类型**。
//
// 为什么不需要内核加机制（三条都能落到现成原语上）:
//
//  1. 评论 = 节点 ⇒ 策略、掩码、后台列表、唯一键、搜索索引全都白拿。
//  2. "被评论的对象" = 一个 **ref 字段**（不是边）: 反向引用（in ->comment.target）
//     现成, ctx.List 能按它筛, 一次 ctx.Create 就能带上目标。
//     代价: ref 的 to 必填（types/utils.go）⇒ 目标类型要写死 ⇒ Options.Types 是
//     "允许被评论的类型"清单, 拼错在 Mount 当场报错。
//  3. 审核 / 可见性 = **站点的策略**（"先审后显""作者看得到自己那条待审"都是站点
//     写一句 where 的事）, 插件不掺和 —— 它只把 4 个字段名当参数。
//
// 插件自己承担的是内核不该管的事: **反垃圾**（频率 / 重复内容 / 链接数 / 长度）与
// **通用端点**（免得每个站手写一遍读写）。
//
// Options 里的类型名与字段名必须与 site.yaml 的声明一致（Mount 时逐个校验, 拼错
// 当场报错 —— 否则症状是"评论列表永远空的", 最难查）。
package comments

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
	"github.com/kran/gcmv3/web"
)

const (
	// DefaultPrefix 默认挂载路径。
	DefaultPrefix = "/api/comments"
	// DefaultType 默认评论类型名。
	DefaultType = "comment"
	// DefaultTargetField 默认"指向被评论对象"的引用字段名。
	DefaultTargetField = "target"
	// DefaultBodyField 默认正文字段名。
	DefaultBodyField = "body"
	// DefaultParentField 默认"回复谁"的引用字段名。
	DefaultParentField = "parent"

	DefaultPageSize = 20
	// MaxPageSize 每页条数上限。
	MaxPageSize = 50
	// DefaultMaxRunes 正文长度上限（按字符数, 中文一个字也是 1）。
	DefaultMaxRunes = 1000
	// DefaultMaxLinks 正文里允许的链接个数。
	DefaultMaxLinks = 2
	// DefaultRateLimit 同一个人的发评论频率（每分钟几条）。
	DefaultRateLimit = 5
	// duplicateWindow 同一个人发一模一样的内容的去重窗口。
	duplicateWindow = 5 * time.Minute
	// rateWindow 频率窗口。
	rateWindow = time.Minute
)

// Options 插件配置。零值可用（除了 Types 必须给）。
type Options struct {
	// Types 允许被评论的类型名（必须真实存在, 且评论类型的 TargetField 必须指向
	// 其中之一）。
	Types []string
	// Type 评论节点类型名（空 = DefaultType）。
	Type string
	// TargetField 指向被评论对象的引用字段（空 = DefaultTargetField）。
	TargetField string
	// BodyField 正文字段（空 = DefaultBodyField）—— 应当是 text/textarea,
	// **不要用 richtext**: 正文会被原样渲染成 HTML 的话, 评论就成了注入口。
	BodyField string
	// ParentField 回复指向的评论字段（空 = DefaultParentField; 置为 "-" 表示
	// 该类型不支持回复, 只发主评论）。
	ParentField string
	// Prefix 挂载路径（空 = DefaultPrefix）。GET 列表, POST 发表。
	Prefix string
	// PageSize 列表每页条数（0 = DefaultPageSize）。
	PageSize int
	// MaxRunes 正文长度上限（0 = DefaultMaxRunes）。
	MaxRunes int
	// MaxLinks 正文里允许的链接个数（0 = DefaultMaxLinks）。
	MaxLinks int
	// RateLimit 频率上限, 每分钟几条（0 = DefaultRateLimit）。
	RateLimit int
	// AllowAnonymous 允许匿名发表。**缺省不允许**（零值 = 要登录 —— 反垃圾的
	// 第一道闸就该是 fail-closed 的那个方向）。
	AllowAnonymous bool
}

// Plugin 装好的评论插件（每个站点一个）。
type Plugin struct {
	site *web.Site
	opts Options
	// commentable 允许被评论的类型名集合。
	commentable map[string]bool

	mu sync.Mutex
	// recent 频率窗口内每次发表的时间（key 见 key()）。
	recent map[string][]time.Time
	// lastBody 最近一次发表的内容与时间（去重; key 同上）。
	lastBody map[string]bodyStamp
}

type bodyStamp struct {
	body string
	at   time.Time
}

// Mount 装上插件: 校验声明 + 挂两个路由。
//
// 幂等: 不建表（评论就是一个节点类型）; 路由重复注册会 panic（框架 fail-loud）——
// 别装两次。
func Mount(site *web.Site, options Options) (*Plugin, error) {
	if site == nil {
		return nil, fmt.Errorf("comments: site is required")
	}
	if len(options.Types) == 0 {
		return nil, fmt.Errorf("comments: 至少声明一个可评论类型（Options.Types 为空 —— 装了也没地方评论）")
	}
	plugin := &Plugin{
		site:        site,
		opts:        withDefaults(options),
		commentable: map[string]bool{},
		recent:      map[string][]time.Time{},
		lastBody:    map[string]bodyStamp{},
	}

	err := plugin.validate()
	if err != nil {
		return nil, err
	}
	err = plugin.mountRoute()
	if err != nil {
		return nil, err
	}
	return plugin, nil
}

// withDefaults 补齐默认值（"字段名"是插件与站点声明的接口, 必须有确定值）。
func withDefaults(options Options) Options {
	if options.Type == "" {
		options.Type = DefaultType
	}
	if options.TargetField == "" {
		options.TargetField = DefaultTargetField
	}
	if options.BodyField == "" {
		options.BodyField = DefaultBodyField
	}
	if options.ParentField == "" {
		options.ParentField = DefaultParentField
	}
	if options.Prefix == "" {
		options.Prefix = DefaultPrefix
	}
	if options.PageSize <= 0 {
		options.PageSize = DefaultPageSize
	}
	if options.PageSize > MaxPageSize {
		options.PageSize = MaxPageSize
	}
	if options.MaxRunes <= 0 {
		options.MaxRunes = DefaultMaxRunes
	}
	if options.MaxLinks < 0 {
		options.MaxLinks = 0
	}
	if options.MaxLinks == 0 {
		options.MaxLinks = DefaultMaxLinks
	}
	if options.RateLimit <= 0 {
		options.RateLimit = DefaultRateLimit
	}
	return options
}

// validate 把"插件与站点声明的接口"逐条核对 —— 拼错在这里爆, 不留到运行期。
func (p *Plugin) validate() error {
	names := make([]string, 0, len(p.opts.Types))
	for _, name := range p.opts.Types {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		_, ok := p.site.Types().Type(name)
		if !ok {
			return fmt.Errorf("comments: 可评论类型 %q 不存在（Options.Types 拼错了？）", name)
		}
		p.commentable[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return fmt.Errorf("comments: 可评论类型全是空串（Options.Types: %v）", p.opts.Types)
	}

	def, ok := p.site.Types().Type(p.opts.Type)
	if !ok {
		return fmt.Errorf("comments: 评论类型 %q 不存在（Options.Type 或 site.yaml 少了一个类型）", p.opts.Type)
	}
	for _, want := range p.requiredFields() {
		field, ok := fieldDef(def, want.name)
		if !ok {
			return fmt.Errorf("comments: 评论类型 %q 没有字段 %q（%s）", p.opts.Type, want.name, want.why)
		}
		if want.ref && field.To == "" {
			return fmt.Errorf("comments: 评论类型 %q 的字段 %q 必须是 ref（现在是 %q）", p.opts.Type, want.name, field.Kind)
		}
		if want.name == p.opts.TargetField && !p.commentable[field.To] {
			return fmt.Errorf("comments: %q 字段指向 %q, 它不在 Options.Types %v 里（能评谁由这一处说了算）",
				p.opts.TargetField, field.To, names)
		}
	}
	return nil
}

type fieldWish struct {
	name string
	ref  bool
	why  string
}

// requiredFields 插件依赖的字段清单（按需 —— 不支持回复就不要求 parent）。
func (p *Plugin) requiredFields() []fieldWish {
	wishes := []fieldWish{
		{p.opts.BodyField, false, "正文没地方放"},
		{p.opts.TargetField, true, "不指明评论哪条内容就成了孤儿"},
	}
	if p.opts.ParentField != "-" {
		wishes = append(wishes, fieldWish{p.opts.ParentField, true, "回复挂不到主评论上（不支持回复就把它设成 \"-\"）"})
	}
	return wishes
}

// fieldDef 按名字找字段声明。
func fieldDef(def types.TypeDef, name string) (types.FieldDef, bool) {
	for _, field := range def.Fields {
		if field.Name == name {
			return field, true
		}
	}
	return types.FieldDef{}, false
}

// mountRoute 挂两个路由（插件自己挂: 装了就一定能用, 不用站点记得接线）。
func (p *Plugin) mountRoute() error {
	p.site.Hook(web.HookBeforeMount, func(s *web.Site) error {
		s.Router().Get(p.opts.Prefix, p.list)
		s.Router().Post(p.opts.Prefix, p.create)
		return nil
	})
	return nil
}

// list GET /api/comments?type=article&target=<id|地址>&page=&size=
//
// 响应: {items, total, page, size, type, target}
//
//	items  已经过读规则/掩码/展开的评论节点（新的在前）—— **平铺**: 回复也在里面
//	       （靠 parent 展开指回主评论）。可见性完全由站点读规则决定, 插件不过滤。
//
// 平铺而不是"主评论 + replies 归并": 归并要么给每条主评论再查一次（N+1）, 要么
// 一次查全部回复再设上限（那就是静默截断）。平铺 + 前端在同页内归并, 跨页的回复
// 显示成"回复某某", 每个数字都诚实。
func (p *Plugin) list(ctx *web.CmsCtx) {
	target, err := p.target(ctx, ctx.Query("type"), ctx.Query("target"))
	if err != nil {
		ctx.Fail(err)
		return
	}
	page := positive(ctx.Query("page"), 1)
	size := min(positive(ctx.Query("size"), p.opts.PageSize), MaxPageSize)
	where := so.P("in", "->"+p.opts.TargetField, []any{target.ID})
	nodes, total, err := ctx.List(core.NodeQuery{Type: p.opts.Type, Where: where, Sort: p.newestFirst()}, size, (page-1)*size)
	if err != nil {
		ctx.Fail(err)
		return
	}
	_ = ctx.Json(http.StatusOK, map[string]any{
		"items":  nodes,
		"total":  total,
		"page":   page,
		"size":   size,
		"type":   target.Type,
		"target": target.ID,
	})
}

// createRequest POST /api/comments 的请求体。
//
//	{type: "article", target: 12 | "about", body: "...", parent: 34, url: ""}
//
// url 是**蜜罐**: 真表单里它不存在（或不可见）, 只有自动填表的机器人会带上它。
type createRequest struct {
	Type   string `json:"type"`
	Target any    `json:"target"`
	Body   string `json:"body"`
	Parent any    `json:"parent"`
	URL    string `json:"url"`
}

// create POST /api/comments —— 反垃圾都在这里, 内容本身交给站点的写规则。
//
// 响应 201: {node}（与框架的写接口同一个形状: 带 masked/editable 的那份）。
func (p *Plugin) create(ctx *web.CmsCtx) {
	var in createRequest
	err := ctx.BindJSON(&in)
	if err != nil {
		ctx.Fail(err)
		return
	}
	if strings.TrimSpace(in.URL) != "" {
		// 蜜罐命中: 不假装成功（静默丢弃会让真用户以为发出去了）——
		// 机器人拿到 400 也无所谓。
		ctx.Fail(web.BadRequest("提交被拒绝"))
		return
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		ctx.Fail(web.BadRequest("评论内容不能为空"))
		return
	}
	if utf8.RuneCountInString(body) > p.opts.MaxRunes {
		ctx.Fail(web.BadRequest("评论太长了（最多 %d 个字符）", p.opts.MaxRunes))
		return
	}
	links := countLinks(body)
	if links > p.opts.MaxLinks {
		ctx.Fail(web.BadRequest("评论里的链接太多了（最多 %d 个）", p.opts.MaxLinks))
		return
	}

	principal, _ := ctx.Principal() // 匿名时 Principal 返回错误（"没有主体"就是匿名这件事本身, 不是故障）
	if principal == nil && !p.opts.AllowAnonymous {
		ctx.Fail(web.Unauthorized("请先登录后再评论"))
		return
	}
	target, err := p.target(ctx, in.Type, in.Target)
	if err != nil {
		ctx.Fail(err)
		return
	}
	fields := core.Fields{
		p.opts.BodyField:   body,
		p.opts.TargetField: target.ID,
	}
	err = p.fillParent(ctx, fields, in.Parent, target)
	if err != nil {
		ctx.Fail(err)
		return
	}
	// 频率与重复都是**看**（只有真的发成功才记账, 见 record）—— 否则一次被拒的
	// 尝试会占掉额度, 连点两下就把自己锁在外面。
	key := p.limitKey(ctx, principal)
	err = p.checkRate(key)
	if err != nil {
		ctx.Fail(err)
		return
	}
	err = p.checkDuplicate(key, body)
	if err != nil {
		ctx.Fail(err)
		return
	}
	// 归属 / 审核状态 / 脱敏都不在这里: 那是站点 OnCreate 的事（插件不认识"谁是
	// 作者""什么状态算通过"—— 站点说一句就够了）。
	node, err := ctx.Create(p.opts.Type, fields)
	if err != nil {
		ctx.Fail(err)
		return
	}
	p.record(key, body)
	_ = ctx.Json(http.StatusCreated, map[string]any{"node": node})
}

// fillParent 把 parent 落到 fields, 并把"回复的回复"归并到顶层 ——
// 层级固定两层（数据上还能再深, 但界面与语义只认两层: 回复永远挂主评论）。
func (p *Plugin) fillParent(ctx *web.CmsCtx, fields core.Fields, raw any, target *core.Node) error {
	if p.opts.ParentField == "-" || raw == nil {
		return nil
	}
	parentID, err := types.ToID(raw)
	if err != nil {
		return web.BadRequest("回复的评论参数不正确")
	}
	if parentID <= 0 {
		return nil
	}
	parent, err := ctx.Get(p.opts.Type, parentID)
	if err != nil {
		return err
	}
	if parent == nil {
		return web.NotFound("要回复的评论不存在")
	}
	if parent.Fields.Int(p.opts.TargetField) != target.ID {
		return web.BadRequest("要回复的评论不在同一条内容下")
	}
	if up := parent.Fields.Int(p.opts.ParentField); up > 0 {
		parentID = up
	}
	fields[p.opts.ParentField] = parentID
	return nil
}

// target 取被评论的对象。
//
// 走 **ctx.Get**（受管读入口）: 看不见的内容回 404（不暴露存在性）, 与"写路径也过
// 读范围"同一条原则 —— 评论一个你看不到的东西没有意义。
func (p *Plugin) target(ctx *web.CmsCtx, typeName string, ref any) (*core.Node, error) {
	typeName = strings.TrimSpace(typeName)
	if typeName == "" {
		return nil, web.BadRequest("缺少 type（被评论内容的类型）")
	}
	if !p.commentable[typeName] {
		return nil, web.BadRequest("该类型不支持评论")
	}
	// ref 是 id（数字）或地址（字符串）—— 两种都直接交给 ctx.Get（它认得）。
	if text, ok := ref.(string); ok && strings.TrimSpace(text) == "" {
		return nil, web.BadRequest("缺少 target（评论哪条内容）")
	}
	if ref == nil {
		return nil, web.BadRequest("缺少 target（评论哪条内容）")
	}
	node, err := ctx.Get(typeName, ref)
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, web.NotFound("内容不存在")
	}
	return node, nil
}

// newestFirst 新的在前（评论没有"排序字段"这种东西, id 就是时间序）。
func (p *Plugin) newestFirst() []so.SortField {
	path, err := so.ParsePath("id")
	if err != nil {
		panic("comments: bad sort path id: " + err.Error())
	}
	return []so.SortField{{Path: path, Desc: true}}
}

// checkRate 频率闸（**只看不记**）: 一个 key 在 rateWindow 内最多 RateLimit 次。
//
// 进程内计数（单实例够用; 多实例要换成共享存储 —— 那时候再说, 不提前造）。
func (p *Plugin) checkRate(key string) error {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.prune(key, now)
	p.recent[key] = kept
	if len(kept) >= p.opts.RateLimit {
		return web.Errorf(http.StatusTooManyRequests, "评论太频繁了，请稍后再试（每分钟最多 %d 条）", p.opts.RateLimit)
	}
	return nil
}

// record 发表**成功之后**记账（频率 + 最近一次内容）。
func (p *Plugin) record(key, body string) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recent[key] = append(p.prune(key, now), now)
	p.lastBody[key] = bodyStamp{body: body, at: now}
}

// prune 丢掉窗口外的时间点（顺手清 map, 不留垃圾; 调用方持锁）。
func (p *Plugin) prune(key string, now time.Time) []time.Time {
	kept := p.recent[key][:0]
	for _, at := range p.recent[key] {
		if now.Sub(at) < rateWindow {
			kept = append(kept, at)
		}
	}
	return kept
}

// checkDuplicate 同一个人连发一模一样的内容 ⇒ 拒（连点两下"发表"的与刷屏的都算）。
// 同样只看不记 —— 记账在 record。
func (p *Plugin) checkDuplicate(key, body string) error {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	last, ok := p.lastBody[key]
	if ok && last.body == body && now.Sub(last.at) < duplicateWindow {
		return web.Conflict("刚刚已经发过一样的内容了")
	}
	return nil
}

// limitKey 反垃圾的计数 key: 登录的按人, 匿名的按来源 IP。
//
// IP 用 RemoteAddr（**不读 X-Forwarded-For**）: 那个头是客户端可伪造的 ⇒ 拿它当
// 限流键等于没限流。反代后面所有人都从一个地址来 ⇒ 匿名场景下额度是全站共享的 ——
// 这是"要不要开匿名评论"要考虑的代价之一。
func (p *Plugin) limitKey(ctx *web.CmsCtx, principal *core.Node) string {
	if principal != nil {
		return "u:" + strconv.FormatInt(principal.ID, 10)
	}
	host, _, err := net.SplitHostPort(ctx.R.RemoteAddr)
	if err != nil {
		host = ctx.R.RemoteAddr
	}
	return "ip:" + host
}

// countLinks 数正文里的链接（http/https 出现次数, 不解析 URL —— 目的是给刷屏
// 加个闸, 不是做 URL 校验）。
func countLinks(body string) int {
	lower := strings.ToLower(body)
	count := 0
	for _, scheme := range []string{"http://", "https://"} {
		count += strings.Count(lower, scheme)
	}
	return count
}

// positive 解析正整数 query（缺省/非法都回 fallback —— page/size 是**展示**参数,
// 不是业务数据, 不为它报错; 越界与非法值最终都只是"看哪一页"）。
func positive(raw string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
