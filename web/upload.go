// 文件上传 —— 落 uploads/, 按年月分目录。
//
//	POST /api/upload    multipart/form-data, 字段名 "file"
//	→ 200 {"path": "/uploads/2026/09/1789837227-9f3a1c05b7e2d816.png"}
//
// 五条规矩:
//
//	① 必须登录 —— 匿名上传是不可接受的滥用面
//	② 扩展名白名单 + **内容嗅探必须与扩展名相符**（`.png` 里装 HTML 进不来）
//	③ **不保留客户端文件名** —— 名字自己生成。于是没有路径穿越、没有编码陷阱、
//	   也没有"文件名里带着上传者真名"的隐私泄漏; 展示名该存在节点字段里,
//	   而不是靠 URL 里的文件名
//	④ 大小上限（默认 10 MiB; 部署层的 client_max_body_size 是另一道）
//	⑤ 落盘 O_EXCL: 撞名不覆盖, 写一半失败就删掉
//
// 服务是 E1 的 /uploads/*: 图片内联, 其余强制下载（第二道防线; 第一道就是这个白名单）。
//
// 已知取舍: 文件落盘与"节点字段里记下这个路径"是两步, 中间失败会留下**孤儿文件**。
// 回收是站点的事（一个脚本扫 uploads/ 与引用比一比就行）—— 框架不做后台 GC。
package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// defaultUploadLimit 单文件上限（10 MiB）。
	defaultUploadLimit = 10 << 20
	// uploadMemory multipart 在内存里保留的上限, 超出部分由标准库落临时文件。
	uploadMemory = 1 << 20
	// uploadField multipart 的字段名。
	uploadField = "file"
)

// uploadTypes 允许上传的扩展名 → 允许的内容类型（嗅探结果必须落在其中）。
//
// 只放两类: 浏览器渲染也安全的图片, 以及反正会被强制下载的文档/音视频。
// **`.svg` / `.html` / `.js` 故意不在里面** —— 它们能在本站域里跑脚本。
//
// 注意: 内容类型的名字是 Go 的 `http.DetectContentType` 的说法, 不是客户端的说法
// （所以同一扩展名可能对上多个名字, 见 .ico / .wav）。UploadContentTypes 有测试逐个核对。
var uploadTypes = map[string][]string{
	".jpg":  {"image/jpeg"},
	".jpeg": {"image/jpeg"},
	".png":  {"image/png"},
	".gif":  {"image/gif"},
	".webp": {"image/webp"},
	".bmp":  {"image/bmp"},
	".ico":  {"image/x-icon", "image/vnd.microsoft.icon"},
	".pdf":  {"application/pdf"},
	".zip":  {"application/zip", "application/x-zip-compressed"},
	".mp4":  {"video/mp4"},
	".webm": {"video/webm", "audio/webm"},
	".mp3":  {"audio/mpeg"},
	".wav":  {"audio/wave", "audio/wav", "audio/x-wav"},
}

// UploadLimit 单文件上传上限。n <= 0 = 关闭上传（路由还在, 但一律 403）。
//
// 默认 10 MiB。要更大的文件请同时改部署层的 client_max_body_size ——
// 反向代理会在应用看到请求之前就切断。
func (s *Site) UploadLimit(n int64) { s.uploadLimit = n }

// apiUpload POST /api/upload
func (s *Site) apiUpload(ctx *CmsCtx) {
	if s.uploadLimit <= 0 {
		ctx.Fail(Forbidden("上传未开启"))
		return
	}
	if ctx.Actor().IsAnonymous() {
		ctx.Fail(Unauthorized("上传需要登录"))
		return
	}
	// 提前套上限: 超大请求在解析 multipart 之前就被切断（否则要先落一堆临时文件）
	ctx.R.Body = http.MaxBytesReader(ctx.W, ctx.R.Body, s.uploadLimit)
	err := ctx.R.ParseMultipartForm(uploadMemory)
	if err != nil {
		ctx.Fail(uploadBodyError(err, s.uploadLimit))
		return
	}
	defer func() {
		// 不清理会在系统临时目录里攒垃圾
		err := ctx.R.MultipartForm.RemoveAll()
		if err != nil {
			slog.Error("web: upload: cleanup temp files", "err", err)
		}
	}()

	file, header, err := ctx.R.FormFile(uploadField)
	if err != nil {
		ctx.Fail(BadRequest("缺少 %q 字段", uploadField))
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	allowed, ok := uploadTypes[ext]
	if !ok {
		ctx.Fail(Errorf(http.StatusUnprocessableEntity, "不支持的文件类型 %q", ext))
		return
	}
	contentType, err := sniffContentType(file)
	if err != nil {
		ctx.Fail(BadRequest("上传内容读不出来: %s", err.Error()))
		return
	}
	if !slices.Contains(allowed, contentType) {
		ctx.Fail(Errorf(http.StatusUnprocessableEntity,
			"文件内容（%s）与扩展名（%s）不符", contentType, ext))
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		ctx.Fail(Internal("上传失败"))
		return
	}

	rel, err := s.saveUpload(file, ext)
	if err != nil {
		ctx.Fail(err)
		return
	}
	_ = ctx.Json(http.StatusOK, map[string]any{"path": "/" + filepath.ToSlash(rel)})
}

// saveUpload 落盘, 返回相对于站点根目录的路径（`uploads/2026/09/….png`）。
func (s *Site) saveUpload(file io.Reader, ext string) (string, error) {
	now := time.Now()
	rel := filepath.Join(uploadsDir, now.Format("2006/01"),
		fmt.Sprintf("%d-%s%s", now.Unix(), randomHex(8), ext))
	dst := filepath.Join(s.basedir, rel)
	err := os.MkdirAll(filepath.Dir(dst), 0o755)
	if err != nil {
		return "", Internal("上传失败")
	}
	// O_EXCL: 名字撞了就失败, 绝不覆盖已有文件
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", Internal("上传失败")
	}
	_, err = io.Copy(out, file)
	if err == nil {
		err = out.Close()
	}
	if err != nil {
		_ = out.Close()
		_ = os.Remove(dst) // 不留半截文件
		return "", Internal("上传失败")
	}
	return rel, nil
}

// sniffContentType 按内容判类型（前 512 字节）并把读位置交给调用方复位。
func sniffContentType(file io.ReadSeeker) (string, error) {
	head := make([]byte, 512)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	if n == 0 {
		return "", errors.New("空文件")
	}
	return http.DetectContentType(head[:n]), nil
}

func uploadBodyError(err error, limit int64) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return Errorf(http.StatusRequestEntityTooLarge, "文件过大（上限 %d MiB）", limit>>20)
	}
	return BadRequest("multipart 表单不合法: %s", err.Error())
}

// randomHex n 字节随机数的十六进制（2n 个字符）。
func randomHex(n int) string {
	buf := make([]byte, n)
	_, err := rand.Read(buf)
	if err != nil {
		panic("web: upload: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
