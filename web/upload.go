// 文件上传 —— 落 uploads/, 默认按年月分目录。
//
//	POST /api/upload    multipart/form-data, 字段名 "file"
//	→ 200 {"path": "/uploads/2026/09/1789837227-9f3a1c05b7e2d816.png"}
//
// 六条规矩:
//
//	① **必须登录** —— 框架的底线（匿名上传是不可接受的滥用面）。要公开上传请自己挂端点
//	② 全站一条**上传规则**（site.Upload）声明: 登录之后还要满足什么业务条件、能传什么、
//	   多大、落哪。不注册 = 默认（任何已登录身份 + 框架白名单 + 站点上限 + 年月目录）
//	③ 扩展名白名单 + **内容嗅探必须与扩展名相符**（`.png` 里装 HTML 进不来）
//	④ **不保留客户端文件名** —— 名字自己生成。于是没有路径穿越、没有编码陷阱、
//	   也没有"文件名里带着上传者真名"的隐私泄漏; 展示名该存在节点字段里
//	⑤ 大小上限（默认 10 MiB; 站点上限是硬顶, 规则的 MaxBytes 只能更小）
//	⑥ 落盘 O_EXCL: 撞名不覆盖, 写一半失败就删
//
// 服务是 E1 的 /uploads/*: 图片内联, 其余强制下载（第二道防线; 第一道就是这个白名单）。
//
// 处理顺序（为什么是流式而不是"先 ParseMultipartForm 再判"）: 规则的输入是**扩展名 +
// 嗅探结果**, 这些在读 part 头与头 512 字节之后就有了 —— 于是在**落盘之前**就能拒掉,
// 既不产生最终文件也不产生临时文件; 规则的 MaxBytes 也能在拷贝时逐字节生效。
//
// 已知取舍: 文件落盘与"节点字段里记下这个路径"是两步, 中间失败会留下**孤儿文件**。
// 回收是站点的事 —— 用 allow.Dir("member/123") 把归属写进路径, 扫盘就能对账。
package web

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// defaultUploadLimit 单文件上限（10 MiB）。
	defaultUploadLimit = 10 << 20
	// uploadOverhead 请求体上限在文件上限之上留的余量（multipart 头与边界）。
	uploadOverhead = 64 << 10
	// uploadField multipart 的字段名。
	uploadField = "file"
	// sniffBytes 判内容类型要读的前缀长度（http.DetectContentType 的要求）。
	sniffBytes = 512
)

// uploadTypes 允许上传的扩展名 → 允许的内容类型（嗅探结果必须落在其中）。
//
// 只放两类: 浏览器渲染也安全的图片, 以及反正会被强制下载的文档/音视频。
// **`.svg` / `.html` / `.js` 故意不在里面** —— 它们能在本站域里跑脚本。
//
// 内容类型的写法是 Go 的 `http.DetectContentType` 的说法, 不是客户端的说法
// （同一扩展名可能对上多个名字, 见 .ico / .wav）。TestUploadSniffTable 逐个核对。
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

// Upload 规则能看到的文件信息（内容已经嗅探过）。
type Upload struct {
	// Name 客户端提交的文件名 —— **不可信**, 不参与落盘, 只给日志/审计用。
	Name string
	Ext  string // 小写扩展名（含点）
	Mime string // 嗅探出来的真实内容类型
}

// UploadRule 全站一条的上传规则。
//
// 上传**不是节点操作**（文件先上来、才可能被节点引用; 同一张图可能被多个节点用）,
// 所以它不挂在类型上 —— 全站一条, 与四动词模型并列存在。
//
// 返回 error = 拒绝（返回 *Error 指定状态码; 别的 error 算服务端错误）。
// 通过时用 allow 声明"允许什么"; 不声明 = 框架默认（全白名单 + 站点上限 + 年月目录）。
//
// **登录是框架的底线**, 规则不必再判"有没有登录" —— 它管的是"登录之后还要满足什么"
// （例如"资料审核通过的会员才行"）。
type UploadRule func(c *CmsCtx, up Upload, allow *UploadAllow) error

// UploadAllow 规则通过时声明允许什么。零值 = 框架默认。
type UploadAllow struct {
	extensions []string
	maxBytes   int64
	dir        string
}

// Extensions 允许的扩展名（"png" 与 ".png" 都收）。必须是框架白名单的**子集** ——
// 想放 `.svg` 进来会在求值时被拒（白名单是安全边界, 不能被规则放宽）。
func (a *UploadAllow) Extensions(exts ...string) {
	for _, ext := range exts {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		a.extensions = append(a.extensions, ext)
	}
}

// MaxBytes 单文件上限。站点 UploadLimit 是**硬顶**, 这里设更大的值没有意义（会被夹到硬顶）。
func (a *UploadAllow) MaxBytes(n int64) { a.maxBytes = n }

// Dir 落盘的子目录（相对 uploads/）, 例如 "member/123"、"avatars"。
//
// 空 = 按年月（"2026/09"）。用它可以按上传者/用途分目录 —— 孤儿文件回收与用量统计
// 都靠路径里的信息, 不用额外一张表。
func (a *UploadAllow) Dir(dir string) { a.dir = dir }

// UploadLimit 单文件上传上限（默认 10 MiB）。n <= 0 = 关闭上传（路由还在, 但一律 403）。
//
// 它是**硬顶**: 规则的 MaxBytes 只能更小。要更大的文件请同时改部署层的
// client_max_body_size —— 反向代理会在应用看到请求之前就切断。
func (s *Site) UploadLimit(n int64) { s.uploadLimit = n }

// Upload 注册全站上传规则（配置期）。重复注册 / 传 nil ⇒ panic。
func (s *Site) Upload(rule UploadRule) {
	if rule == nil {
		panic("web: Site.Upload(nil)")
	}
	if s.started {
		panic("web: Site.Upload: must be registered before Setup")
	}
	if s.uploadRule != nil {
		panic("web: Site.Upload: upload rule already registered")
	}
	s.uploadRule = rule
}

// 一次上传实际生效的允许范围。
type uploadAllowance struct {
	extensions map[string]bool // nil = 框架全白名单
	maxBytes   int64
	dir        string
}

func (a uploadAllowance) allows(ext string) bool {
	if a.extensions == nil {
		return true
	}
	return a.extensions[ext]
}

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
	ctx.R.Body = http.MaxBytesReader(ctx.W, ctx.R.Body, s.uploadLimit+uploadOverhead)
	reader, err := ctx.R.MultipartReader()
	if err != nil {
		ctx.Fail(BadRequest("需要 multipart/form-data: %s", err.Error()))
		return
	}
	part, err := filePart(reader)
	if err != nil {
		ctx.Fail(err)
		return
	}
	defer part.Close()

	ext := strings.ToLower(filepath.Ext(part.FileName()))
	head, err := sniffPart(part)
	if err != nil {
		ctx.Fail(err)
		return
	}
	up := Upload{Name: part.FileName(), Ext: ext, Mime: http.DetectContentType(head)}

	// 规则先跑: 被拒的上传不该落到磁盘上（这正是流式处理的好处）
	allowance, err := s.uploadAllowance(ctx, up)
	if err != nil {
		ctx.Fail(err)
		return
	}
	allowed, ok := uploadTypes[ext]
	if !ok {
		ctx.Fail(Errorf(http.StatusUnprocessableEntity, "不支持的文件类型 %q", ext))
		return
	}
	if !allowance.allows(ext) {
		ctx.Fail(Errorf(http.StatusUnprocessableEntity, "当前身份不允许上传 %s", ext))
		return
	}
	if !slices.Contains(allowed, up.Mime) {
		ctx.Fail(Errorf(http.StatusUnprocessableEntity,
			"文件内容（%s）与扩展名（%s）不符", up.Mime, ext))
		return
	}
	rel, err := s.saveUpload(io.MultiReader(bytes.NewReader(head), part), ext, allowance)
	if err != nil {
		ctx.Fail(err)
		return
	}
	_ = ctx.Json(http.StatusOK, map[string]any{"path": "/" + filepath.ToSlash(rel)})
}

// uploadAllowance 求这次上传的允许范围（没注册规则 = 框架默认）。
func (s *Site) uploadAllowance(c *CmsCtx, up Upload) (uploadAllowance, error) {
	allowance := uploadAllowance{maxBytes: s.uploadLimit}
	if s.uploadRule == nil {
		return allowance, nil
	}
	allow := &UploadAllow{}
	err := s.uploadRule(c, up, allow)
	if err != nil {
		return uploadAllowance{}, err
	}
	// 校验规则声明 —— 拼错的扩展名/越界的目录是配置错误, 当场响亮（不静默放宽）
	for _, ext := range allow.extensions {
		if _, ok := uploadTypes[ext]; !ok {
			return uploadAllowance{}, fmt.Errorf(
				"web: upload rule allows %q, not in the framework whitelist", ext)
		}
		if allowance.extensions == nil {
			allowance.extensions = map[string]bool{}
		}
		allowance.extensions[ext] = true
	}
	if allow.maxBytes > 0 && allow.maxBytes < allowance.maxBytes {
		allowance.maxBytes = allow.maxBytes
	}
	if allow.dir != "" {
		dir, err := cleanUploadDir(allow.dir)
		if err != nil {
			return uploadAllowance{}, err
		}
		allowance.dir = dir
	}
	return allowance, nil
}

// cleanUploadDir 校验规则的目录: 必须是相对 uploads/ 的干净路径（不穿出去）。
func cleanUploadDir(dir string) (string, error) {
	cleaned := path.Clean(strings.TrimSpace(strings.ReplaceAll(dir, `\`, "/")))
	switch {
	case cleaned == "." || cleaned == "":
		return "", nil
	case path.IsAbs(cleaned), strings.HasPrefix(cleaned, "../"),
		strings.HasSuffix(cleaned, "/.."), cleaned == "..":
		return "", fmt.Errorf("web: upload rule dir %q escapes uploads/", dir)
	}
	return cleaned, nil
}

// filePart 找 multipart 里第一个 file 字段（其它字段跳过 —— 客户端可能顺手带点别的）。
func filePart(reader *multipart.Reader) (*multipart.Part, error) {
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, BadRequest("缺少 %q 字段", uploadField)
		}
		if err != nil {
			return nil, uploadReadError(err)
		}
		if part.FormName() == uploadField {
			return part, nil
		}
		_ = part.Close()
	}
}

// sniffPart 读前 sniffBytes 字节并返回（内容为空 ⇒ 400）。
func sniffPart(part io.Reader) ([]byte, error) {
	head := make([]byte, sniffBytes)
	n, err := io.ReadFull(part, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, uploadReadError(err)
	}
	if n == 0 {
		return nil, BadRequest("上传内容是空的")
	}
	return head[:n], nil
}

// saveUpload 落盘, 返回相对于站点根目录的路径（`uploads/2026/09/….png`）。
//
// 大小在**拷贝时**数: multipart 的 part 头不带长度, 只能一边写一边数
// （LimitReader 多给一个字节, 于是"刚好超一点"也判得出来）。
func (s *Site) saveUpload(body io.Reader, ext string, allowance uploadAllowance) (string, error) {
	now := time.Now()
	dir := filepath.Join(uploadsDir, allowance.dir, now.Format("2006/01"))
	rel := filepath.Join(dir, fmt.Sprintf("%d-%s%s", now.Unix(), randomHex(8), ext))
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
	written, err := io.Copy(out, io.LimitReader(body, allowance.maxBytes+1))
	if err == nil && written > allowance.maxBytes {
		err = Errorf(http.StatusRequestEntityTooLarge,
			"文件过大（上限 %d MiB）", allowance.maxBytes>>20)
	}
	if err == nil {
		err = out.Close()
	}
	if err != nil {
		_ = out.Close()
		_ = os.Remove(dst) // 不留半截文件
		return "", uploadReadError(err)
	}
	return rel, nil
}

// uploadReadError 把读取途中的错误翻成对外错误: 请求体超上限 ⇒ 413, 别的 ⇒ 400。
func uploadReadError(err error) error {
	var structured *Error
	if errors.As(err, &structured) {
		return structured
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return Errorf(http.StatusRequestEntityTooLarge, "上传内容过大")
	}
	return BadRequest("读取上传内容失败: %s", err.Error())
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
