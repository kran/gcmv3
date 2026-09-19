package web

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
)

// 各类"最小合法文件"的前若干字节 —— 用来核对 http.DetectContentType 到底认哪些。
var sniffSamples = map[string][]byte{
	".png":  {0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13, 'I', 'H', 'D', 'R'},
	".jpg":  {0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00},
	".jpeg": {0xff, 0xd8, 0xff, 0xe1, 0x00, 0x10, 'E', 'x', 'i', 'f', 0x00},
	".gif":  []byte("GIF89a" + "\x01\x00\x01\x00\x00\x00\x00"),
	".webp": []byte("RIFF\x2a\x00\x00\x00WEBPVP8 "),
	".bmp":  append([]byte("BM"), make([]byte, 26)...),
	".ico":  {0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x10, 0x10},
	".pdf":  []byte("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n"),
	".zip":  []byte("PK\x03\x04\x14\x00\x00\x00"),
	".mp4":  append(append([]byte{0, 0, 0, 24}, []byte("ftypmp42")...), make([]byte, 12)...),
	".webm": {0x1a, 0x45, 0xdf, 0xa3, 0x93, 0x42, 0x82, 0x88},
	".mp3":  []byte("ID3\x03\x00\x00\x00\x00\x00\x00"),
	".wav":  append([]byte("RIFF\x24\x00\x00\x00WAVE"), []byte("fmt ")...),
}

// 每个白名单扩展名都必须**真的能被嗅探对上** —— 这份表是安全边界, 猜不得。
func TestUploadSniffTable(t *testing.T) {
	var bad []string
	for ext, allowed := range uploadTypes {
		sample, ok := sniffSamples[ext]
		if !ok {
			bad = append(bad, ext+" 没有样本（新加的类型必须补样本, 否则守卫是猜的）")
			continue
		}
		sniffed := http.DetectContentType(sample)
		if !contains(allowed, sniffed) {
			bad = append(bad, ext+" 嗅探到 "+sniffed)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("白名单与嗅探表对不上（安全边界, 不能靠猜）:\n  %s", strings.Join(bad, "\n  "))
	}
	// 样本表里不该有白名单外的类型
	for ext := range sniffSamples {
		if _, ok := uploadTypes[ext]; !ok {
			t.Fatalf("样本 %s 不在白名单里", ext)
		}
	}
}

// 上传成功: 落盘在 uploads/年/月/ 下, 名字自己生成, 且能立刻被 /uploads 服务到。
func TestUploadOK(t *testing.T) {
	site := newPolicySite(t)
	site.Setup() // 建目录 + 挂路由
	token := memberToken(t, site)

	got := uploadFile(t, site, token, "photo.png", sniffSamples[".png"], nil)
	if got.Code != http.StatusOK {
		t.Fatalf("上传 = %d %q", got.Code, got.Body.String())
	}
	path := uploadPath(t, got.Body.String())
	if !strings.HasPrefix(path, "/uploads/") {
		t.Fatalf("path = %q", path)
	}
	// 年月分目录
	parts := strings.Split(strings.TrimPrefix(path, "/uploads/"), "/")
	if len(parts) != 3 {
		t.Fatalf("该按 年/月/文件名 落盘: %q", path)
	}
	if len(parts[2]) < 30 || !strings.HasSuffix(parts[2], ".png") {
		t.Fatalf("文件名该是 时间戳-随机.ext: %q", parts[2])
	}
	// 磁盘上真的有, 且能被服务
	onDisk := filepath.Join(site.basedir, filepath.FromSlash(strings.TrimPrefix(path, "/")))
	raw, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("磁盘上没有: %v", err)
	}
	if !bytes.Equal(raw, sniffSamples[".png"]) {
		t.Fatal("内容不一致")
	}
	served := do(t, site, http.MethodGet, path)
	if served.Code != http.StatusOK || served.Body.Len() == 0 {
		t.Fatalf("/uploads 服务 = %d", served.Code)
	}
	if served.Header().Get("Content-Disposition") != "" {
		t.Fatalf("图片该内联: %q", served.Header().Get("Content-Disposition"))
	}
}

// 客户端文件名完全不参与落盘: 穿越、奇怪字符、真名都不进路径。
func TestUploadIgnoresClientFilename(t *testing.T) {
	site := newPolicySite(t)
	site.Setup()
	token := memberToken(t, site)

	for _, name := range []string{
		"../../../../etc/passwd.png",
		`C:\Users\张三\我的照片.png`,
		"照片 名字 (1).png",
		strings.Repeat("x", 300) + ".png",
	} {
		got := uploadFile(t, site, token, name, sniffSamples[".png"], nil)
		if got.Code != http.StatusOK {
			t.Fatalf("%q 上传 = %d %q", name, got.Code, got.Body.String())
		}
		path := uploadPath(t, got.Body.String())
		if strings.Contains(path, "..") || strings.Contains(path, "passwd") ||
			strings.Contains(path, "照片") || strings.Contains(path, "xxx") {
			t.Fatalf("客户端文件名泄漏进路径: %q", path)
		}
		onDisk := filepath.Join(site.basedir, filepath.FromSlash(strings.TrimPrefix(path, "/")))
		if !strings.HasPrefix(filepath.Clean(onDisk), filepath.Clean(site.basedir)) {
			t.Fatalf("落盘跑到站点外面了: %q", onDisk)
		}
	}
}

// 同样的内容传两次 ⇒ 两个不同文件（不覆盖）。
func TestUploadNoOverwrite(t *testing.T) {
	site := newPolicySite(t)
	site.Setup()
	token := memberToken(t, site)
	first := uploadPath(t, uploadFile(t, site, token, "a.png", sniffSamples[".png"], nil).Body.String())
	second := uploadPath(t, uploadFile(t, site, token, "a.png", sniffSamples[".png"], nil).Body.String())
	if first == second {
		t.Fatalf("两次上传该是两个文件: %q", first)
	}
	for _, path := range []string{first, second} {
		onDisk := filepath.Join(site.basedir, filepath.FromSlash(strings.TrimPrefix(path, "/")))
		if _, err := os.Stat(onDisk); err != nil {
			t.Fatalf("%q 不在: %v", path, err)
		}
	}
}

// 第一道防线: 白名单外的扩展名根本进不来（.svg/.html/.js 是能在本站域跑脚本的东西）。
func TestUploadRejectsDangerousTypes(t *testing.T) {
	site := newPolicySite(t)
	site.Setup()
	token := memberToken(t, site)

	for _, name := range []string{"evil.svg", "evil.html", "evil.js", "evil.php", "evil", "evil.png.exe"} {
		got := uploadFile(t, site, token, name, []byte("<svg onload=alert(1)>"), nil)
		if got.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%q = %d %q", name, got.Code, got.Body.String())
		}
	}
}

// 第二道防线: 内容与扩展名必须相符（.png 里装 HTML）。
func TestUploadRejectsMismatchedContent(t *testing.T) {
	site := newPolicySite(t)
	site.Setup()
	token := memberToken(t, site)

	cases := map[string][]byte{
		"假装是 png 的 html": []byte("<!doctype html><script>alert(1)</script>"),
		"假装是 png 的文本":    []byte("just some text, definitely not a png"),
		"假装是 zip 的 png":  sniffSamples[".png"],
		"假装是 jpg 的 gif":  sniffSamples[".gif"],
		"空文件":            {},
		"只有 3 个字节":       {1, 2, 3},
	}
	for name, content := range cases {
		ext := ".png"
		if strings.Contains(name, "zip") {
			ext = ".zip"
		}
		if strings.Contains(name, "jpg") {
			ext = ".jpg"
		}
		got := uploadFile(t, site, token, "x"+ext, content, nil)
		if got.Code < 400 || got.Code >= 500 {
			t.Fatalf("%s = %d %q", name, got.Code, got.Body.String())
		}
	}
}

// 匿名不能传; 关闭上传后 403。
func TestUploadAuthAndDisabled(t *testing.T) {
	site := newPolicySite(t)
	site.Setup()
	anon := uploadFile(t, site, "", "a.png", sniffSamples[".png"], nil)
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 = %d %q", anon.Code, anon.Body.String())
	}
	token := memberToken(t, site)
	site.UploadLimit(0)
	got := uploadFile(t, site, token, "a.png", sniffSamples[".png"], nil)
	if got.Code != http.StatusForbidden {
		t.Fatalf("关闭上传 = %d %q", got.Code, got.Body.String())
	}
}

// 超大 ⇒ 413（在解析 multipart 之前就被切断）。
func TestUploadTooLarge(t *testing.T) {
	site := newPolicySite(t)
	site.Setup()
	site.UploadLimit(2 << 10)
	token := memberToken(t, site)
	big := append(bytes.Repeat(sniffSamples[".png"], 100), make([]byte, 4096)...)
	got := uploadFile(t, site, token, "a.png", big, nil)
	if got.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 = %d %q", got.Code, got.Body.String())
	}
}

// 缺 file 字段 ⇒ 400。
func TestUploadMissingField(t *testing.T) {
	site := newPolicySite(t)
	site.Setup()
	token := memberToken(t, site)
	got := uploadFile(t, site, token, "a.png", sniffSamples[".png"], func(w *multipart.Writer) string {
		return "other"
	})
	if got.Code != http.StatusBadRequest {
		t.Fatalf("字段名不对 = %d %q", got.Code, got.Body.String())
	}
}

// ── 工具 ──

// uploadFile 造一个 multipart 请求打过去。field 为 nil 时用默认字段名。
func uploadFile(t *testing.T, site *Site, token, filename string, content []byte,
	field func(*multipart.Writer) string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	name := uploadField
	if field != nil {
		name = field(writer)
	}
	part, err := writer.CreateFormFile(name, filename)
	if err != nil {
		t.Fatal(err)
	}
	_, err = part.Write(content)
	if err != nil {
		t.Fatal(err)
	}
	err = writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/upload", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	site.Setup().ServeHTTP(recorder, request)
	return recorder
}

func memberToken(t *testing.T, site *Site) string {
	t.Helper()
	id, err := site.Engine().RegisterAuth(nil, "member", "email", "a@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "frontend", id)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func uploadPath(t *testing.T, body string) string {
	t.Helper()
	var payload struct {
		Path string `json:"path"`
	}
	err := json.Unmarshal([]byte(body), &payload)
	if err != nil || payload.Path == "" {
		t.Fatalf("响应里没有 path (%v): %q", err, body)
	}
	return payload.Path
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
