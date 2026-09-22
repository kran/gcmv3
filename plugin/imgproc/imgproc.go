// Package imgproc 图片缩放/裁剪插件 —— 经 `HookServeFile` 生效（不装 = 原图直出）。
//
//	imgproc.Mount(site, imgproc.Options{BaseURL: cfg.OSSBucket})
//
// 参数只有一种写法（**阿里云 OSS 那套** —— 模板/小程序本来就按它拼 URL）:
//
//	?x-oss-process=image/resize,w_300,h_200,m_lfit
//	?x-oss-process=image/resize,w_300,m_fill
//
//	m=lfit  等比缩进 w×h 的框（不裁、不放大小图）; 只给一边时另一边按比例算
//	m=fill  填满 w×h 后居中裁剪（正好这个尺寸）; **必须给 w 和 h**
//	m 不写 = lfit（与 OSS 一致）
//
// 旧的 `?w=&h=&mode=&fmt=` 写法**已移除**: 传了就报错（静默忽略参数比报错更难查 ——
// 页面看着正常, 图就是没处理）。
//
// 两种部署形态（只差一个配置）:
//
//	空         本地处理: 按参数解码/缩放, 缓存到同目录 .cache/ 后直出
//	配 BaseURL 交给外部存储（OSS/CDN）: URL 由 URL() 拼上桶前缀,
//	           打到本站的带参数请求 **302 到桶**（见 redirectToBase）
//
// 302 这条不是多余: 页面 URL 是 URL() 拼的, 正常情况下不会打到本站 —— 但换配置之前
// 写进 DB 的正文、旧缓存页面、手写的相对路径都会打到本站, 静默原图直出正是最难查的
// 那类"参数不生效"。
//
// 三条"软失败"（都是原图直出, 不让一张图把页面搞崩）—— 只适用于**本地处理**:
//
//	原文件不存在 / 格式不支持（webp、avif 之类 imaging 不认）/ 处理或编码失败
//
// 缓存**不做 GC**（越用越多是站点的事: 清 .cache 目录随时安全, 下次请求会重建）。
package imgproc

import (
	"fmt"
	"html/template"
	"image"
	"log/slog"
	"net/http"
	"net/url"

	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"github.com/disintegration/imaging"

	"github.com/kran/gcmv3/types"
	"github.com/kran/gcmv3/web"
)

// Options 插件配置。
type Options struct {
	// BaseURL 图片的外部存储前缀（OSS/CDN, 如 "https://bucket.oss-cn-shenzhen.aliyuncs.com"）。
	// 配了 ⇒ 本地不解码（远端处理）: URL() 拼上前缀, 本站的带参数请求 302 到桶;
	// 空 ⇒ 本地处理。
	BaseURL string
}

// contentImageWidth 正文图的最大宽度（lfit 只缩不放 ⇒ 小图不动）。
const contentImageWidth = 1200

// Plugin 装好的实例（每个站点一个 —— URL() 要用**自己**的 base）。
//
// 以前这里是**包级全局** `mounted` ⇒ 多站进程（web.HostMux）里后挂的站会把前一个覆盖
// ⇒ viicn 的页面用上 lizhiqi 的桶（图片全 404）。**站点相关的东西不能放包级**。
type Plugin struct{ baseURL string }

// Mount 装上插件（注册 HookServeFile —— 图片处理/转发），返回实例供 URL() 用。
//
// 装上之后的用法（站点侧）:
//
//	img := imgproc.Mount(site, imgproc.Options{BaseURL: cfg.OssBucket})
//	... img.URL(path, 600, 400, "fill")
func Mount(s *web.Site, options Options) *Plugin {
	base := strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	instance := &Plugin{baseURL: base}
	instance.registerTemplateFuncs(s)
	// 两种模式都挂: 本地模式处理图片, OSS 模式把带参数的请求转给桶
	// （配了 BaseURL 就**不挂** hook 的话, 打到本站的 ?x-oss-process= 会被静默丢掉）
	s.Hook(web.HookServeFile, instance.serveFile)
	return instance
}

// registerTemplateFuncs 把图片相关的模板函数挂到站点上 —— **插件自己的功能就由插件注册**,
// 站点不用在 setup 里再抄一遍（抄一遍就有第二份, 两站一改一忘 —— 踩过两次）。
//
//	oss   拼图片 URL（模板: {{ .Fields.cover | oss 800 450 "cover" }}）
//	rich  富文本 → HTML, 并**把正文里写死的 <img src="/uploads/…"> 也过一遍 imgproc**
//	      （模板里的 oss 只管字段值, 碰不到正文 HTML; OSS 部署下那些相对路径会打到本站）
//
// 两个函数都**覆盖**同名内置/站点版本: 装了图片插件, 图片相关的事就归它。
func (p *Plugin) registerTemplateFuncs(s *web.Site) {
	render := s.Render()
	// 注册失败（名字冲突/签名不对）是配置期错误 ⇒ fail-loud
	mustFunc := func(name string, fn any) {
		err := render.Func(name, fn)
		if err != nil {
			panic("imgproc: 注册模板函数 " + name + ": " + err.Error())
		}
	}
	mustFunc("oss", func(_ *web.CmsCtx, args ...any) (string, error) {
		return p.ossURL(args...), nil
	})
	mustFunc("rich", func(_ *web.CmsCtx, value any) (template.HTML, error) {
		return p.rich(value), nil
	})
}

// ossURL 模板 `oss` 的实现。三处 v2 遗留物都在这里兜住:
//
//  1. **管道把值放最后**: `{{ .Fields.cover | oss 800 450 "cover" }}` 等价于
//     `oss(800, 450, "cover", 路径)` —— 取 args[0] 会拿到 800 ⇒ 拼出空 URL（图全空, 踩过）。
//  2. **值可能是 template.HTML**（站点配置的返回值）⇒ 只断言 string 会落空。
//  3. **模式名**: v2 的 fit|cover|crop → v3 的 lfit|fill（写错图片服务返 400 ⇒ 图裂）。
func (p *Plugin) ossURL(args ...any) string {
	if len(args) == 0 {
		return ""
	}
	asText := func(value any) (string, bool) {
		switch typed := value.(type) {
		case string:
			return typed, true
		case template.HTML:
			return string(typed), true
		}
		return "", false
	}
	path, mode := "", ""
	width, height := 0, 0
	rest := args
	if text, ok := asText(args[0]); ok {
		path = text
		rest = args[1:]
	} else {
		path, _ = asText(args[len(args)-1])
		rest = args[:len(args)-1]
	}
	for index, arg := range rest {
		if text, ok := asText(arg); ok {
			if mode == "" {
				mode = text
			}
			continue
		}
		number, err := types.ToID(arg)
		if err != nil {
			continue
		}
		switch index {
		case 0:
			width = int(number)
		case 1:
			height = int(number)
		}
	}
	switch mode {
	case "fit":
		mode = "lfit"
	case "cover", "crop":
		mode = "fill"
	}
	return p.URL(path, width, height, mode)
}

// rich 富文本 → HTML, 并把正文里的相对图片过一遍 imgproc。
//
// 用真解析器（x/net/html）而不是正则: 属性顺序/单双引号/自闭合这些正则迟早出错, 而正文是
// 编辑器产出的机器 HTML。**解析失败原样返回**（宁可没优化, 不能丢内容）+ 日志。
func (p *Plugin) rich(value any) template.HTML {
	raw, ok := value.(string)
	if !ok || raw == "" {
		return template.HTML("") //nolint:gosec // 空
	}
	if !strings.Contains(raw, "<img") {
		return template.HTML(raw) //nolint:gosec // 站点自己的富文本
	}
	nodes, err := html.ParseFragment(strings.NewReader(raw), nil)
	if err != nil {
		slog.Warn("imgproc: 富文本解析失败（原样输出）", "err", err)
		return template.HTML(raw) //nolint:gosec
	}
	var out strings.Builder
	for _, node := range nodes {
		p.rewriteImages(node)
		err = html.Render(&out, node)
		if err != nil {
			slog.Warn("imgproc: 富文本渲染失败（原样输出）", "err", err)
			return template.HTML(raw) //nolint:gosec
		}
	}
	return template.HTML(out.String()) //nolint:gosec // 站点自己的富文本
}

// rewriteImages 就地把 img 的相对 src 换成过 imgproc 的地址（绝对地址/已处理的跳过）。
func (p *Plugin) rewriteImages(node *html.Node) {
	if node.Type == html.ElementNode && node.Data == "img" {
		for index := range node.Attr {
			attr := &node.Attr[index]
			if attr.Key != "src" || attr.Val == "" {
				continue
			}
			if strings.HasPrefix(attr.Val, "http://") || strings.HasPrefix(attr.Val, "https://") ||
				strings.Contains(attr.Val, "x-oss-process=") {
				continue
			}
			// 正文图给一个够宽的上限: lfit 只缩不放 ⇒ 小图不受影响
			attr.Val = p.URL(attr.Val, contentImageWidth, 0, "lfit")
		}
		if !hasAttr(node, "loading") {
			node.Attr = append(node.Attr, html.Attribute{Key: "loading", Val: "lazy"})
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		p.rewriteImages(child)
	}
}

func hasAttr(node *html.Node, key string) bool {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return true
		}
	}
	return false
}

// URL 拼一个图片地址（模板用它 ⇒ 本地/OSS 两种部署只差配置）。
//
//	img.URL("/uploads/a.jpg", 300, 0, "lfit") ⇒
//	  本地: /uploads/a.jpg?x-oss-process=image/resize,w_300,m_lfit
//	  OSS : https://bucket…/uploads/a.jpg?x-oss-process=image/resize,w_300,m_lfit
//
// **实例方法**（不是包级全局）: 多站进程里每个站有自己的桶, 谁也不能覆盖谁。
func (p *Plugin) URL(path string, width, height int, mode string) string {
	base := ""
	if p != nil {
		base = p.baseURL
	}
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path + ProcessQuery(width, height, mode)
}

// ProcessQuery 只拼参数部分（w/h/m）; 没有尺寸就返回空串（原图）。
func ProcessQuery(width, height int, mode string) string {
	if width <= 0 && height <= 0 {
		return ""
	}
	segments := []string{"image/resize"}
	if width > 0 {
		segments = append(segments, "w_"+strconv.Itoa(width))
	}
	if height > 0 {
		segments = append(segments, "h_"+strconv.Itoa(height))
	}
	if mode = normalizeMode(mode); mode != "" {
		segments = append(segments, "m_"+mode)
	}
	return "?x-oss-process=" + strings.Join(segments, ",")
}

// maxImgDim 单边最大像素// maxImgDim 单边最大像素（防"用参数让人去解码一张 40000×40000 的图"）。
const maxImgDim = 4000

// serveFile HookServeFile 的处理器。
//
//	没配 BaseURL —— 本地处理（解码/缩放/缓存）
//	配了 BaseURL —— 带参数就 302 到桶, 本地一个字节都不解码
func (p *Plugin) serveFile(ctx *web.CmsCtx, filePath *string) error {
	if p.baseURL != "" {
		return p.redirectToBase(ctx, filePath)
	}
	params, ok, err := parseParams(ctx.R)
	if err != nil {
		// 不写响应 —— serveFiles 是统一的错误出口（这里写了会双写）。
		// 用 web.Error: 参数问题是客户端的错（400）, 不是服务端故障（500）。
		return web.BadRequest("图片参数不合法: %s", err.Error())
	}
	if !ok {
		return nil // 没带参数: 原图直出
	}
	if _, err := os.Stat(*filePath); err != nil {
		return nil // 原文件不存在 —— 交给兜底 404
	}
	cachePath := cacheFile(*filePath, params, ctx.R.URL.Path)
	if st, err := os.Stat(cachePath); err == nil && st.Mode().IsRegular() {
		*filePath = cachePath // 命中缓存
		return nil
	}
	src, err := imaging.Open(*filePath, imaging.AutoOrientation(true))
	if err != nil {
		return nil // 不支持的格式（webp/avif…）—— 原图直出
	}
	dst, err := process(src, params)
	if err != nil {
		return nil
	}
	err = save(cachePath, dst)
	if err != nil {
		// 不支持的格式是**预期**的软失败（webp/avif）; 这里落盘失败是意外, 要留痕,
		// 否则只会表现为"图片没被裁" —— 静默降级最难查。
		slog.Warn("imgproc: save failed, serving original", "path", *filePath, "err", err)
		return nil
	}
	*filePath = cachePath
	return nil
}

// redirectToBase 把带处理参数的本地请求 302 给桶。
//
// 为什么配了 BaseURL 还要管本地请求: URL() 拼出来的地址本来就指向桶; 但**换配置之前**
// 写进 DB 的正文、旧缓存页面、站长/小程序手写的相对路径都会打到本站 —— 那些请求带着
// x-oss-process 却没人处理, 图就悄悄是原图（本插件自己反复踩的就是这种静默降级）。
// 本地没有这个能力, 有能力的是远端 ⇒ 参数原样交过去。
//
// 三处刻意的取舍:
//
//  1. **只在带参数时重定向**。不带参数的请求本地有文件就直出 —— uploads 是本地落盘的,
//     桶里可能还没同步, 重定向过去反而成了 404（把能用的行为改坏了）。
//  2. **302 不是 301**: 桶/CDN 换前缀是常态, 301 会被浏览器永久记住。
//  3. **参数不在这里校验**（除了本站自己的旧参数名）: 远端认的 process 比我们多
//     （image/format,webp 之类）, 拿本地白名单去卡它等于把远端能处理的请求判成 400。
func (p *Plugin) redirectToBase(ctx *web.CmsCtx, filePath *string) error {
	query := ctx.R.URL.Query()
	err := rejectLegacy(query)
	if err != nil {
		// 旧参数名是**本站的**约定错, 远端也不认识它（会当没看见 ⇒ 又是静默）—— 自己报
		return web.BadRequest("图片参数不合法: %s", err.Error())
	}
	if strings.TrimSpace(query.Get("x-oss-process")) == "" {
		return nil // 没带处理参数: 本地原图直出
	}
	target := p.baseURL + ctx.R.URL.Path
	if ctx.R.URL.RawQuery != "" {
		target += "?" + ctx.R.URL.RawQuery
	}
	ctx.Redirect(http.StatusFound, target)
	*filePath = "" // 已经应答 ⇒ 约定: serveFiles 见空路径即返回（否则文件内容会被追加到 302 后面）
	return nil
}

// save 原子落盘: 先写临时文件再改名。
//
// 直接 imaging.Save 到目标路径有个真问题: 两个并发请求同时处理同一张图, 一个正在写的
// 文件会被另一个（或这个请求自己的 ServeFile）读到 —— 半张图。改名是原子的。
func save(cachePath string, img image.Image) error {
	err := os.MkdirAll(filepath.Dir(cachePath), 0o755)
	if err != nil {
		return err
	}
	// 临时文件**必须带正确的扩展名** —— imaging.Save 是按扩展名挑编码器的,
	// 随机后缀（.tmp-1234）会让它认不出格式, 于是"原子落盘"变成"永远失败 + 静默原图"。
	tmp, err := os.CreateTemp(filepath.Dir(cachePath), ".tmp-*"+filepath.Ext(cachePath))
	if err != nil {
		return err
	}
	name := tmp.Name()
	err = tmp.Close()
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	err = imaging.Save(img, name)
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	// 目标扩展名决定编码格式 ⇒ 临时文件不能带随机后缀（imaging 按扩展名挑编码器）
	err = os.Rename(name, cachePath)
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// cacheFile 缓存路径（原文件同目录下的 .cache/）。
func cacheFile(filePath string, p params, reqPath string) string {
	base := filepath.Base(reqPath)
	baseNoExt := strings.TrimSuffix(base, filepath.Ext(base))
	return filepath.Join(filepath.Dir(filePath), ".cache",
		fmt.Sprintf("%s-%dx%d-%s%s", p.mode, p.width, p.height, baseNoExt, filepath.Ext(base)))
}

type params struct {
	width, height int
	mode          string // lfit | fill
}

// rejectLegacy 旧参数名**响亮报错**（静默忽略 = 页面看着正常、图就是没处理, 最难查）。
// 本地与 OSS 两种模式都要查: 远端同样不认识 ?w=, 只会当没看见。
func rejectLegacy(q url.Values) error {
	for _, legacy := range []string{"w", "h", "mode", "fmt"} {
		if q.Get(legacy) != "" {
			return fmt.Errorf(
				"旧参数 ?%s= 已移除 —— 请用 ?x-oss-process=image/resize,w_300,h_200,m_lfit|m_fill", legacy)
		}
	}
	return nil
}

// parseParams 解析查询串。三态: (p, true) 要处理 / (p, false) 没带参数 / err 参数非法。
func parseParams(r *http.Request) (params, bool, error) {
	q := r.URL.Query()
	err := rejectLegacy(q)
	if err != nil {
		return params{}, false, err
	}
	process := strings.TrimSpace(q.Get("x-oss-process"))
	if process == "" {
		return params{}, false, nil
	}
	p := params{mode: "lfit"}
	sawResize := false
	for segment := range strings.SplitSeq(process, ",") {
		segment = strings.TrimSpace(segment)
		switch {
		case segment == "":
		case segment == "image/resize":
			sawResize = true
		case strings.HasPrefix(segment, "image/"):
			return params{}, false, fmt.Errorf("不支持的 process %q（只支持 image/resize）", segment)
		case strings.HasPrefix(segment, "w_"):
			n, err := dim(strings.TrimPrefix(segment, "w_"))
			if err != nil {
				return params{}, false, err
			}
			p.width = n
		case strings.HasPrefix(segment, "h_"):
			n, err := dim(strings.TrimPrefix(segment, "h_"))
			if err != nil {
				return params{}, false, err
			}
			p.height = n
		case strings.HasPrefix(segment, "m_"):
			mode := normalizeMode(strings.TrimPrefix(segment, "m_"))
			if mode == "" {
				return params{}, false, fmt.Errorf("不支持的 m_%s（只支持 lfit / fill）",
					strings.TrimPrefix(segment, "m_"))
			}
			p.mode = mode
		default:
			return params{}, false, fmt.Errorf("不支持的 process 段 %q", segment)
		}
	}
	if !sawResize {
		return params{}, false, fmt.Errorf("缺少 image/resize")
	}
	if p.width == 0 && p.height == 0 {
		return params{}, false, fmt.Errorf("resize 需要 w_ 或 h_")
	}
	// fill 必须两边都给: 一边填不满, 按比例推出来的是 lfit 的形状 —— 猜尺寸不如报错
	if p.mode == "fill" && (p.width == 0 || p.height == 0) {
		return params{}, false, fmt.Errorf("m_fill 需要同时给 w_ 和 h_")
	}
	return p, true, nil
}

// dim 解析一个边长（正整数, 单边上限防"用参数让人解码 40000×40000 的图"）。
func dim(raw string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("边长必须是正整数")
	}
	if n > maxImgDim {
		return 0, fmt.Errorf("边长超过上限 %d", maxImgDim)
	}
	return n, nil
}

// normalizeMode 归一化模式名（空 = 不写, 调用方按默认处理）。
func normalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "":
		return ""
	case "lfit":
		return "lfit"
	case "fill":
		return "fill"
	}
	return ""
}

// process 按模式处理:
//
//	lfit 等比缩进框（不放大: 小图原样）
//	fill 填满后居中裁剪（正好 w×h）
func process(src image.Image, p params) (image.Image, error) {
	bounds := src.Bounds()
	if p.mode == "fill" {
		return imaging.Fill(src, p.width, p.height, imaging.Center, imaging.Lanczos), nil
	}
	width, height := fitInto(p.width, p.height, bounds)
	if width >= bounds.Dx() && height >= bounds.Dy() {
		return src, nil // 不放大: 已经装得下
	}
	return imaging.Fit(src, width, height, imaging.Lanczos), nil
}

// fitInto 缺的那一边按原图比例补齐。
func fitInto(width, height int, bounds image.Rectangle) (int, int) {
	if width == 0 {
		width = int(float64(bounds.Dx()) * float64(height) / float64(bounds.Dy()))
	}
	if height == 0 {
		height = int(float64(bounds.Dy()) * float64(width) / float64(bounds.Dx()))
	}
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	return width, height
}
