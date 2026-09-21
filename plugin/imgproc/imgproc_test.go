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
	err := os.WriteFile(filepath.Join(basedir, "site.yaml"), []byte(`
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
	Mount(site, Options{})
	site.Setup() // 建 static/uploads 目录
	return site, basedir
}

// writePNG 在站点里放一张 w×h 的 PNG，返回它的 URL 路径。
func writePNG(t *testing.T, basedir, name string, width, height int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := range width {
		for y := range height {
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
	got := fetch(t, site, url+"?x-oss-process=image/resize,w_60")
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
	// 缓存名用的是**请求参数**（w_60, h 缺省 ⇒ 60x0）, 不是解析后的尺寸 ——
	// 这样缓存命中判断不必先解码原图。同一组参数 ⇒ 同一个缓存文件。
	if entries[0].Name() != "lfit-60x0-b.png" {
		t.Fatalf("缓存文件名叫法变了: %q", entries[0].Name())
	}
	// 再请求: 还是这个结果（走缓存; 内容一致）
	again := fetch(t, site, url+"?x-oss-process=image/resize,w_60")
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
		{"?x-oss-process=image/resize,w_40,h_20,m_fill", 40, 20}, // 填满并裁剪 ⇒ 正好
		{"?x-oss-process=image/resize,w_40,h_20,m_lfit", 20, 20}, // 等比放进框（小图不放大小 ✗
		{"?x-oss-process=image/resize,w_40", 40, 40},             // 只给一边 ⇒ 按比例
		{"?x-oss-process=image/resize,h_20", 20, 20},             // 只给 h 同理
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
	for _, query := range []string{"?x-oss-process=image/resize,w_0", "?x-oss-process=image/resize,w_99999", "?mode=nope&w=10", "?fmt=gif&w=10", "?x-oss-process=image/nope"} {
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
	got := fetch(t, site, "/uploads/fake.png?x-oss-process=image/resize,w_10")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "not an image") {
		t.Fatalf("不支持处理时该原图直出: %d %q", got.Code, got.Body.String())
	}
}

func decodeAny(raw []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	return img, err
}

// 旧参数**响亮报错**（静默忽略 = 页面正常但图没处理, 最难查）。
func TestLegacyParamsRejected(t *testing.T) {
	site, basedir := newSite(t)
	url := writePNG(t, basedir, "legacy.png", 120, 60)
	for _, legacy := range []string{"?w=60", "?h=60", "?mode=cover", "?fmt=jpg", "?w=60&h=60&mode=crop"} {
		got := fetch(t, site, url+legacy)
		if got.Code != http.StatusBadRequest {
			t.Fatalf("%s 该 400（旧参数已移除）, 实际 %d: %s", legacy, got.Code, got.Body.String())
		}
		if !strings.Contains(got.Body.String(), "x-oss-process") {
			t.Fatalf("错误信息该给出正确写法: %s", got.Body.String())
		}
	}
}

// m_fill 必须两边都给（只给一边推出来的是 lfit 的形状 —— 猜不如报错）。
func TestFillRequiresBothSides(t *testing.T) {
	site, basedir := newSite(t)
	url := writePNG(t, basedir, "fill.png", 120, 60)
	got := fetch(t, site, url+"?x-oss-process=image/resize,w_40,m_fill")
	if got.Code != http.StatusBadRequest {
		t.Fatalf("m_fill 缺 h_ 该 400, 实际 %d", got.Code)
	}
	// 不支持的 m_ 也要报错（只支持 lfit / fill）
	got = fetch(t, site, url+"?x-oss-process=image/resize,w_40,h_40,m_cover")
	if got.Code != http.StatusBadRequest {
		t.Fatalf("m_cover 该 400（只支持 lfit/fill —— cover 是旧口径）, 实际 %d", got.Code)
	}
}

// URL() / ProcessQuery(): 模板拼地址用（本地与 OSS 只差配置）。
func TestURLBuilder(t *testing.T) {
	cases := []struct {
		width, height int
		mode          string
		want          string
	}{
		{300, 0, "lfit", "?x-oss-process=image/resize,w_300,m_lfit"},
		{300, 200, "fill", "?x-oss-process=image/resize,w_300,h_200,m_fill"},
		{300, 200, "cover", "?x-oss-process=image/resize,w_300,h_200"}, // 旧名不认 ⇒ 不带 m_
		{0, 0, "lfit", ""}, // 没尺寸 = 原图
	}
	for _, c := range cases {
		if got := ProcessQuery(c.width, c.height, c.mode); got != c.want {
			t.Errorf("ProcessQuery(%d,%d,%q) = %q, 期望 %q", c.width, c.height, c.mode, got, c.want)
		}
	}
	// 没装插件也要能拼（模板不该依赖装没装）
	if got := URL("uploads/a.jpg", 300, 0, "lfit"); got != "/uploads/a.jpg?x-oss-process=image/resize,w_300,m_lfit" {
		t.Errorf("URL() = %q", got)
	}
}

// 配了 BaseURL ⇒ 本地不挂 hook（交给远端处理）, URL() 带上前缀。
func TestBaseURLSkipsLocalProcessing(t *testing.T) {
	site, _ := newSite(t)
	Mount(site, Options{BaseURL: "https://bucket.example.com/"})
	t.Cleanup(func() { mounted = nil })
	if got := URL("/uploads/a.jpg", 300, 0, "lfit"); got !=
		"https://bucket.example.com/uploads/a.jpg?x-oss-process=image/resize,w_300,m_lfit" {
		t.Fatalf("配了 BaseURL 的 URL() = %q", got)
	}
	mounted = nil
}
