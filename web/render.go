// 服务端渲染（模板）—— 内容站的那套：级联取模板 + 片段 + 函数注册。
//
// 为什么在 web 而不是单独包: 模板函数要拿**请求上下文**（CmsCtx）才能走受管读入口,
// 而那是 web 的东西。精简口径（对照 v2 的 434 行）:
//
//	留: 级联取模板（node--{type}.html → node.html）· 无缓存（改文件下一请求生效）
//	    Partial/partialOr（片段 + 缺片段兜底）· Func（站点注册）· fail-loud
//	去: 查询原语注入（模板里写查询是魔法面 —— 站点自己注册可测的 Go 函数）
//	    图原语（core 的 traverse/expand 已承担）· sprig（大依赖 + 另一个项目的语义）
//	    布局继承（页面自己 partial 组织, 不引入 layout 概念）
//
// 模板函数的两条约定:
//
//	签名可带 `*CmsCtx` 当第一个参数 —— 注册时校验, 调用时自动注入。
//	  这是框架唯一给的便利, 也是**强制走读入口**的抓手: 数据函数里必须 ctx.Get/List,
//	  模板因此永远不绕过读规则与掩码。
//	返回 (值, error) —— 错误由渲染层统一成 500; 不玩 panic 那套隐式控制流。
package web

import (
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

// RenderOptions 渲染引擎配置。
type RenderOptions struct {
	// Root 模板目录（相对 baseDir; 空 = "templates"）。
	Root string
	// Funcs 站点自定义模板函数（与 Render.Func 等价, 图省事在这里一起给）。
	Funcs map[string]any
	// AssetBase 资产前缀（相对路径 /uploads/... 补成 AssetBase+/uploads/... ——
	// 空 = 原样; 与 imgproc 的 OSS base 是同一个概念）。
	AssetBase string
}

// Render 渲染引擎（每个站点一个）。
type Render struct {
	site  *Site
	root  string
	asset string
	funcs map[string]reflect.Value
}

// NewRender 建渲染引擎。Root 不存在 ⇒ 直接报错（站点忘了放 templates 目录时,
// 应该在建站期就知道, 而不是等第一个请求 500）。
func NewRender(site *Site, options RenderOptions) (*Render, error) {
	if site == nil {
		return nil, fmt.Errorf("web: render: site is required")
	}
	root := strings.TrimSpace(options.Root)
	if root == "" {
		root = "templates"
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(site.BaseDir(), root)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("web: render: 模板目录不可用: %s", root)
	}
	out := &Render{
		site: site, root: root,
		asset: strings.TrimRight(strings.TrimSpace(options.AssetBase), "/"),
		funcs: map[string]reflect.Value{},
	}
	for name, fn := range options.Funcs {
		err := out.Func(name, fn)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Func 注册模板函数（站点/插件扩展点, 如 own 的 url / oss / 数据查询）。
//
// 允许两种签名: `func(*CmsCtx, ...) (值, error)`（自动注入当前请求上下文）
// 与 `func(...) (值, error)`（纯函数）。注册时就校验 —— 写错在这里报, 不留到渲染。
func (r *Render) Func(name string, fn any) error {
	if name == "" {
		return fmt.Errorf("web: render: 函数名不能为空")
	}
	value := reflect.ValueOf(fn)
	if value.Kind() != reflect.Func {
		return fmt.Errorf("web: render: %s 不是函数", name)
	}
	kind := value.Type()
	// 返回必须是 (值, error) —— 错误统一由渲染层处理
	if kind.NumOut() != 2 || !kind.Out(1).Implements(reflect.TypeFor[error]()) {
		return fmt.Errorf("web: render: %s 的签名要是 func(...) (值, error)", name)
	}
	if kind.NumIn() > 0 && kind.In(0) == reflect.TypeFor[*CmsCtx]() {
		// 带上下文: 调用时注入
	}
	r.funcs[name] = value
	return nil
}

// Render 按候选序取第一个存在的模板执行（级联: node--{type}.html → node.html）。
func (r *Render) Render(ctx *CmsCtx, w io.Writer, candidates []string, data any) error {
	for _, name := range candidates {
		full := filepath.Join(r.root, filepath.Clean("/"+name))
		if _, err := os.Stat(full); err != nil {
			continue
		}
		return r.execute(ctx, w, full, data)
	}
	return fmt.Errorf("web: render: 找不到模板（试过 %s）", strings.Join(candidates, " / "))
}

// Partial 渲染片段（独立解析执行; 缺片段返回哨兵错误, 供 partialOr 兜底）。
func (r *Render) Partial(ctx *CmsCtx, name string, data any) (template.HTML, error) {
	full := filepath.Join(r.root, filepath.Clean("/"+name))
	if _, err := os.Stat(full); err != nil {
		return "", errPartialNotFound
	}
	var buf strings.Builder
	err := r.execute(ctx, &buf, full, data)
	if err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil //nolint:gosec // 模板自己产出的 HTML
}

// execute 单个模板文件独立解析执行（无缓存 ⇒ 改文件下一请求生效）。
func (r *Render) execute(ctx *CmsCtx, w io.Writer, full string, data any) error {
	tpl, err := template.New(filepath.Base(full)).Funcs(r.funcMap(ctx)).ParseFiles(full)
	if err != nil {
		return fmt.Errorf("web: render: 解析 %s: %w", full, err)
	}
	err = tpl.Execute(w, data)
	if err != nil {
		return fmt.Errorf("web: render: 执行 %s: %w", full, err)
	}
	return nil
}

var errPartialNotFound = fmt.Errorf("web: render: 片段不存在")

// funcMap 内置函数 + 站点注册的函数（带 *CmsCtx 的自动注入当前请求上下文）。
func (r *Render) funcMap(ctx *CmsCtx) template.FuncMap {
	out := template.FuncMap{
		"rich":    r.rich,
		"excerpt": excerpt,
		"date":    formatDate,
		"default": defaultValue,
		"join":    joinStrings,
		"asset":   r.assetURL,
		"partial": func(name string, data any) (template.HTML, error) { return r.Partial(ctx, name, data) },
		"partialOr": func(name string, fallback string, data any) template.HTML {
			rendered, err := r.Partial(ctx, name, data)
			if err != nil {
				return template.HTML(template.HTMLEscapeString(fallback)) //nolint:gosec
			}
			return rendered
		},
	}
	for name, fn := range r.funcs {
		out[name] = bindContext(ctx, fn)
	}
	return out
}

// bindContext 把 `func(*CmsCtx, ...)` 包装成模板能直接调的形态（其余原样）。
func bindContext(ctx *CmsCtx, fn reflect.Value) any {
	kind := fn.Type()
	withCtx := kind.NumIn() > 0 && kind.In(0) == reflect.TypeFor[*CmsCtx]()
	return func(args ...any) (any, error) {
		in := make([]reflect.Value, 0, len(args)+1)
		if withCtx {
			in = append(in, reflect.ValueOf(ctx))
		}
		for _, arg := range args {
			in = append(in, reflect.ValueOf(arg))
		}
		// 参数个数/类型不对是**模板写错了** ⇒ 让它响亮地报出来（不静默给零值）
		if len(in) != kind.NumIn() {
			return nil, fmt.Errorf("模板函数参数个数不对: 给了 %d 个, 需要 %d 个", len(args), kind.NumIn()-boolToInt(withCtx))
		}
		out := fn.Call(in)
		if !out[1].IsNil() {
			return nil, out[1].Interface().(error)
		}
		return out[0].Interface(), nil
	}
}

// ── 内置函数（只留内容站必然要、写错了会踩坑的那几个）──

// rich 富文本 → 安全 HTML（模板里必须用, 否则 <p> 被转义成字面量）,
// 并把站内相对资源补成带前缀的绝对地址（AssetBase）。
var richSrcPattern = regexp.MustCompile(`(src|href)="(/(?:uploads|static)/[^"]+)"`)

func (r *Render) rich(value any) template.HTML {
	text, _ := value.(string)
	if text == "" {
		return ""
	}
	if r.asset != "" {
		text = richSrcPattern.ReplaceAllString(text, `$1="`+r.asset+`$2"`)
	}
	return template.HTML(text) //nolint:gosec // 富文本在写路径上已过白名单（见上传/写规则）
}

// asset 相对路径 → 带 AssetBase 的地址（空 base = 原样）。
func (r *Render) assetURL(value any) string {
	path, _ := value.(string)
	if path == "" {
		return ""
	}
	if r.asset == "" || strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return r.asset + path
}

// excerpt 富文本 → 纯文本 + 按**字符**截断（按字节中文会烂）。
func excerpt(value any, limit int) string {
	text, _ := value.(string)
	text = htmlTagPattern.ReplaceAllString(text, "")
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if limit > 0 && len(runes) > limit {
		return strings.TrimSpace(string(runes[:limit])) + "…"
	}
	return text
}

var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)

// formatDate Unix 秒 → 本地时间（默认 2006-01-02; 第二参可选 layout）。
func formatDate(value any, layout ...string) string {
	seconds := toSeconds(value)
	if seconds <= 0 {
		return ""
	}
	shape := "2006-01-02"
	if len(layout) > 0 && layout[0] != "" {
		shape = layout[0]
	}
	return time.Unix(seconds, 0).Local().Format(shape)
}

func toSeconds(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	case string:
		var parsed int64
		_, err := fmt.Sscanf(typed, "%d", &parsed)
		if err != nil {
			return 0
		}
		return parsed
	}
	return 0
}

// defaultValue 空值兜底（sprig 里最常用的那个）: `{{ .X | default "—" }}`。
func defaultValue(fallback, value any) any {
	if isEmptyValue(value) {
		return fallback
	}
	return value
}

func isEmptyValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case int64:
		return typed == 0
	case int:
		return typed == 0
	case bool:
		return !typed
	}
	return false
}

// joinStrings 切片 → 字符串（Go 模板没有 `join`, 列表标签页必用）。
func joinStrings(sep string, value any) string {
	items := reflect.ValueOf(value)
	if !items.IsValid() || (items.Kind() != reflect.Slice && items.Kind() != reflect.Array) {
		if value == nil {
			return ""
		}
		return fmt.Sprintf("%v", value)
	}
	parts := make([]string, 0, items.Len())
	for i := range items.Len() {
		parts = append(parts, fmt.Sprintf("%v", items.Index(i).Interface()))
	}
	return strings.Join(parts, sep)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
