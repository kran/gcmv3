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
// BaseURL: 配了就把图片交给外部存储（OSS/CDN）—— 本插件**不处理**（远端那套
// 参数自己会生效）, URL 由 URL() 拼。空 = 本地处理并缓存到同目录 .cache/。
//
// 三条"软失败"（都是原图直出, 不让一张图把页面搞崩）:
//
//	原文件不存在 / 格式不支持（webp、avif 之类 imaging 不认）/ 处理或编码失败
//
// 缓存**不做 GC**（越用越多是站点的事: 清 .cache 目录随时安全, 下次请求会重建）。
package imgproc

import (
	"fmt"
	"image"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/disintegration/imaging"

	"github.com/kran/gcmv3/web"
)

// Options 插件配置。
type Options struct {
	// BaseURL 图片的外部存储前缀（OSS/CDN, 如 "https://bucket.oss-cn-shenzhen.aliyuncs.com"）。
	// 配了 ⇒ 本地不处理（远端处理）, URL 由 URL() 拼; 空 ⇒ 本地处理。
	BaseURL string
}

// plugin 装好的实例（URL() 要用 base）。
type plugin struct{ baseURL string }

var mounted *plugin

// Mount 装上插件（注册 HookServeFile —— 图片处理）。
func Mount(s *web.Site, options Options) {
	base := strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	mounted = &plugin{baseURL: base}
	if base != "" {
		// 交给远端: 本地不做任何处理（远端按同一套 x-oss-process 参数处理）
		return
	}
	s.Hook(web.HookServeFile, ServeFile)
}

// URL 拼一个图片地址（模板用它 ⇒ 本地/OSS 两种部署只差配置）。
//
//	imgproc.URL("/uploads/a.jpg", 300, 0, "lfit") ⇒
//	  本地: /uploads/a.jpg?x-oss-process=image/resize,w_300,m_lfit
//	  OSS : https://bucket…/uploads/a.jpg?x-oss-process=image/resize,w_300,m_lfit
//
// 没装插件也安全（按本地相对路径拼）—— 模板不该依赖"装没装插件"。
func URL(path string, width, height int, mode string) string {
	base := ""
	if mounted != nil {
		base = mounted.baseURL
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

// ServeFile HookServeFile 的处理器。
func ServeFile(ctx *web.CmsCtx, filePath *string) error {
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

// parseParams 解析查询串。三态: (p, true) 要处理 / (p, false) 没带参数 / err 参数非法。
func parseParams(r *http.Request) (params, bool, error) {
	q := r.URL.Query()
	for _, legacy := range []string{"w", "h", "mode", "fmt"} {
		if q.Get(legacy) != "" {
			return params{}, false, fmt.Errorf(
				"旧参数 ?%s= 已移除 —— 请用 ?x-oss-process=image/resize,w_300,h_200,m_lfit|m_fill", legacy)
		}
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
