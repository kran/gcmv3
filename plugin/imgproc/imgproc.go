// Package imgproc 图片缩放/裁剪插件 —— 经 `HookServeFile` 生效（不装 = 原图直出）。
//
//	imgproc.Mount(site)
//
// /static 与 /uploads 每次请求都 Fire `HookServeFile`; 本插件:
//
//	没带裁剪参数   不动 filePath（原图直出）
//	带了裁剪参数   处理 → 落盘到同目录 `.cache/` → 把 filePath 指到缓存
//	              （后续的 ServeFile 就服务缓存文件; 下次同参数直接命中）
//
// 参数（查询串）:
//
//	?w=300&h=200&mode=cover|fit|crop&fmt=jpg|png
//	?x-oss-process=image/resize,w_300,m_lfit    ← 兼容阿里云 OSS 那套写法
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

// maxImgDim 单边最大像素（防"用参数让人去解码一张 40000×40000 的图"）。
const maxImgDim = 4000

// Mount 装上插件（注册 HookServeFile —— 图片处理）。
func Mount(s *web.Site) {
	s.Hook(web.HookServeFile, ServeFile)
}

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
	ext := filepath.Ext(base)
	if p.format != "" {
		ext = "." + p.format
	}
	return filepath.Join(filepath.Dir(filePath), ".cache",
		fmt.Sprintf("%dx%d-%s-%s%s", p.width, p.height, p.mode, baseNoExt, ext))
}

type params struct {
	width, height int
	mode          string // cover | fit | crop
	format        string // "" | jpg | png
}

// parseParams 解析查询串。三态: (p, true) 要处理 / (p, false) 没带参数 / err 参数非法。
func parseParams(r *http.Request) (params, bool, error) {
	q := r.URL.Query()
	if process := q.Get("x-oss-process"); process != "" {
		return parseOSSProcess(process)
	}
	rawW, rawH := q.Get("w"), q.Get("h")
	if rawW == "" && rawH == "" {
		return params{}, false, nil
	}
	p := params{mode: "cover"}
	var err error
	if rawW != "" {
		p.width, err = dim(rawW)
		if err != nil {
			return params{}, false, fmt.Errorf("invalid w")
		}
	}
	if rawH != "" {
		p.height, err = dim(rawH)
		if err != nil {
			return params{}, false, fmt.Errorf("invalid h")
		}
	}
	switch mode := strings.ToLower(q.Get("mode")); mode {
	case "":
	case "cover", "fit", "crop":
		p.mode = mode
	default:
		return params{}, false, fmt.Errorf("invalid mode")
	}
	switch format := strings.ToLower(q.Get("fmt")); format {
	case "":
	case "jpg", "jpeg":
		p.format = "jpg"
	case "png":
		p.format = "png"
	default:
		return params{}, false, fmt.Errorf("invalid fmt")
	}
	return p, true, nil
}

func dim(raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxImgDim {
		return 0, fmt.Errorf("dimension out of range")
	}
	return n, nil
}

// parseOSSProcess 兼容阿里云 OSS 的 `x-oss-process=image/resize,w_300,m_lfit` 写法
// （小程序/老前端常按这个拼 URL）。
func parseOSSProcess(process string) (params, bool, error) {
	p := params{mode: "cover"}
	for segment := range strings.SplitSeq(process, ",") {
		segment = strings.TrimSpace(segment)
		switch {
		case segment == "":
		case segment == "image/resize":
		case strings.HasPrefix(segment, "image/"):
			return params{}, false, fmt.Errorf("unsupported process %q", segment)
		case strings.HasPrefix(segment, "w_"):
			n, err := dim(strings.TrimPrefix(segment, "w_"))
			if err != nil {
				return params{}, false, fmt.Errorf("invalid w_")
			}
			p.width = n
		case strings.HasPrefix(segment, "h_"):
			n, err := dim(strings.TrimPrefix(segment, "h_"))
			if err != nil {
				return params{}, false, fmt.Errorf("invalid h_")
			}
			p.height = n
		case segment == "m_fill":
			p.mode = "cover"
		case segment == "m_lfit":
			p.mode = "fit"
		default:
			return params{}, false, fmt.Errorf("unsupported process segment %q", segment)
		}
	}
	if p.width == 0 && p.height == 0 {
		return params{}, false, fmt.Errorf("resize needs w_ or h_")
	}
	return p, true, nil
}

// process 按模式处理: cover=填充裁剪 / fit=等比放进框 / crop=居中裁剪。
//
// 只给了一边时, 另一边按比例算（等比缩放）。
func process(src image.Image, p params) (image.Image, error) {
	bounds := src.Bounds()
	switch p.mode {
	case "crop":
		if p.width == 0 || p.height == 0 {
			return nil, fmt.Errorf("crop requires both w and h")
		}
		return imaging.CropCenter(src, p.width, p.height), nil
	case "fit":
		p.width, p.height = fitInto(p.width, p.height, bounds)
		return imaging.Fit(src, p.width, p.height, imaging.Lanczos), nil
	default:
		p.width, p.height = fitInto(p.width, p.height, bounds)
		return imaging.Fill(src, p.width, p.height, imaging.Center, imaging.Lanczos), nil
	}
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
