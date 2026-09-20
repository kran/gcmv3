package imgproc

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kran/gcmv3/web"
)

// 站点夹具: 一个只有 uploads 的最小站点（挂 imgproc）。
//
// 返回 basedir —— Site 故意不暴露 BaseDir（插件不该关心站点的目录布局）,
// 测试自己造目录, 所以自己留着路径。
func newSite(t *testing.T) (*web.Site, string) {
	t.Helper()
	basedir := t.TempDir()
	err := os.WriteFile(filepath.Join(basedir, "types.yaml"), []byte(`
types:
  article:
    fields:
      - { name: title, kind: text }
`), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	site, err := web.Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = site.Close() })
	Mount(site)
	site.Setup() // 建 static/uploads 目录
	return site, basedir
}

// writePNG 在站点里放一张 w×h 的 PNG，返回它的 URL 路径。
func writePNG(t *testing.T, basedir, name string, width, height int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	err := png.Encode(&buf, img)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(basedir, "uploads")
	err = os.MkdirAll(dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	return "/uploads/" + name
}

func fetch(t *testing.T, site *web.Site, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	site.Setup().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

// 没带参数 ⇒ 原图直出（不动 filePath）。
func TestServeOriginalWithoutParams(t *testing.T) {
	site, basedir := newSite(t)
	url := writePNG(t, basedir, "a.png", 120, 60)
	got := fetch(t, site, url)
	if got.Code != http.StatusOK {
		t.Fatalf("原图 = %d", got.Code)
	}
	img, err := png.Decode(bytes.NewReader(got.Body.Bytes()))
	if err != nil {
		t.Fatalf("原图解不开: %v", err)
	}
	if img.Bounds().Dx() != 120 || img.Bounds().Dy() != 60 {
		t.Fatalf("原图尺寸被改了: %v", img.Bounds())
	}
	if _, err := os.Stat(filepath.Join(basedir, "uploads", ".cache")); !os.IsNotExist(err) {
		t.Fatal("没参数不该生成缓存")
	}
}

// 带 w ⇒ 等比缩到宽 60（高按比例 30）, 并落进 .cache; 再请求一次走缓存。
func TestServeResized(t *testing.T) {
	site, basedir := newSite(t)
	url := writePNG(t, basedir, "b.png", 120, 60)
	got := fetch(t, site, url+"?w=60")
	if got.Code != http.StatusOK {
		t.Fatalf("缩放 = %d %q", got.Code, got.Body.String())
	}
	img, err := png.Decode(bytes.NewReader(got.Body.Bytes()))
	if err != nil {
		t.Fatalf("缩放结果解不开: %v", err)
	}
	if img.Bounds().Dx() != 60 || img.Bounds().Dy() != 30 {
		t.Fatalf("尺寸 = %v（该是 60x30）", img.Bounds())
	}
	// 缓存落盘
	entries, err := os.ReadDir(filepath.Join(basedir, "uploads", ".cache"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("缓存目录 = %v (%v)", entries, err)
	}
	// 缓存名用的是**请求参数**（w=60,h 缺省 ⇒ 60x0）, 不是解析后的尺寸 ——
	// 这样缓存命中判断不必先解码原图。同一组参数 ⇒ 同一个缓存文件。
	if entries[0].Name() != "60x0-cover-b.png" {
		t.Fatalf("缓存文件名叫法变了: %q", entries[0].Name())
	}
	// 再请求: 还是这个结果（走缓存; 内容一致）
	again := fetch(t, site, url+"?w=60")
	if again.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), again.Body.Bytes()) {
		t.Fatal("第二次请求该走缓存且结果一致")
	}
}

// crop / fit / fmt 与 OSS 写法。
func TestServeModes(t *testing.T) {
	site, basedir := newSite(t)
	url := writePNG(t, basedir, "c.png", 100, 100)
	cases := []struct {
		query  string
		width  int
		height int
	}{
		{"?w=40&h=20&mode=crop", 40, 20},
		{"?w=40&h=20&mode=fit", 20, 20},   // 等比放进 40x20 的框 ⇒ 20x20
		{"?w=40&h=20&mode=cover", 40, 20}, // 填充裁剪 ⇒ 正好 40x20
		{"?w=30&fmt=jpg", 30, 30},         // 转 jpg 也放大成功（尺寸按比例）
		{"?x-oss-process=image/resize,w_50,m_lfit", 50, 50},
	}
	for _, test := range cases {
		got := fetch(t, site, url+test.query)
		if got.Code != http.StatusOK {
			t.Fatalf("%s = %d %q", test.query, got.Code, got.Body.String())
		}
		img, err := decodeAny(got.Body.Bytes())
		if err != nil {
			t.Fatalf("%s 解不开: %v", test.query, err)
		}
		if img.Bounds().Dx() != test.width || img.Bounds().Dy() != test.height {
			t.Fatalf("%s 尺寸 = %v, 期望 %dx%d", test.query, img.Bounds(), test.width, test.height)
		}
	}
}

// 非法参数 ⇒ 400（通过 serveFiles 的统一错误出口）。
func TestServeInvalidParams(t *testing.T) {
	site, basedir := newSite(t)
	url := writePNG(t, basedir, "d.png", 40, 40)
	for _, query := range []string{"?w=0", "?w=99999", "?mode=nope&w=10", "?fmt=gif&w=10", "?x-oss-process=image/nope"} {
		got := fetch(t, site, url+query)
		if got.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %q", query, got.Code, got.Body.String())
		}
	}
}

// 软失败: 不支持的格式（这里用一段假 PNG 头之外的二进制）⇒ 原图直出。
func TestServeUnsupportedFormatFallsBack(t *testing.T) {
	site, basedir := newSite(t)
	dir := filepath.Join(basedir, "uploads")
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	// 名字是 .png 但内容不是图片（serveFiles 不做内容嗅探 —— 上传才做）
	err = os.WriteFile(filepath.Join(dir, "fake.png"), []byte("not an image at all"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	got := fetch(t, site, "/uploads/fake.png?w=10")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "not an image") {
		t.Fatalf("不支持处理时该原图直出: %d %q", got.Code, got.Body.String())
	}
}

func decodeAny(raw []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	return img, err
}
