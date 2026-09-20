package backup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	so "github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
	"github.com/kran/gcmv3/web"
)

// 夹具: 真站点（真 sqlite 文件）+ 装好的备份插件 + 一个 owner 身份的令牌。
func newHarness(t *testing.T) (*web.Site, http.Handler, string, string) {
	t.Helper()
	basedir := t.TempDir()
	err := os.WriteFile(filepath.Join(basedir, "types.yaml"), []byte(`
types:
  staff:
    capabilities:
      authentication: { roles: [秘书处] }
    fields:
      - { name: name, kind: text }
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(basedir, "backups")
	os.MkdirAll(backups, 0o755)

	site, err := web.Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = site.Close() })
	site.Type("staff").OnRead(func(_ *web.CmsCtx, where *so.Where, _ *web.Grant) error {
		*where = so.P("true")
		return nil
	})
	Mount(site, Options{BackupsDir: backups})

	// owner 身份（后台门是 owner||admin；备份面板要求登录即可）
	staffID, err := site.Engine().RegisterAuth(nil, "staff", "username", "boss",
		core.Fields{}, &core.Node{Type: "staff",
			Fields: core.Fields{"name": "站长", "roles": []any{types.RoleOwner}}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := site.Engine().CreateSession(nil, "staff", staffID)
	if err != nil {
		t.Fatal(err)
	}
	return site, site.Setup(), token, backups
}

func do(t *testing.T, handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// 端点都在后台门后面（没登录 ⇒ 401/403, 不是 200）。
func TestBackupEndpointsRequireAuth(t *testing.T) {
	_, handler, _, _ := newHarness(t)
	response := do(t, handler, http.MethodGet, "/admin/backup", "", "")
	if response.Code == http.StatusOK {
		t.Fatalf("未登录不该能列备份: %d", response.Code)
	}
}

// 备份 → 列表 → 下载 → 删除（VACUUM INTO 一致快照）。
func TestBackupLifecycle(t *testing.T) {
	_, handler, token, backups := newHarness(t)

	// ① 立即备份
	response := do(t, handler, http.MethodPost, "/admin/backup", token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("备份 = %d: %s", response.Code, response.Body.String())
	}
	var created struct {
		OK   bool `json:"ok"`
		Item struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"item"`
	}
	decode(t, response, &created)
	if !created.OK || !strings.HasPrefix(created.Item.Name, "backup-") {
		t.Fatalf("备份响应: %#v", created)
	}
	if created.Item.Size <= 0 {
		t.Fatalf("备份文件该非空: %#v", created)
	}
	if _, err := os.Stat(filepath.Join(backups, created.Item.Name)); err != nil {
		t.Fatalf("备份文件该落盘: %v", err)
	}

	// ② 列表（倒序, 只认 backup-*.db）
	response = do(t, handler, http.MethodGet, "/admin/backup", token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("列表 = %d: %s", response.Code, response.Body.String())
	}
	var listed struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	decode(t, response, &listed)
	if len(listed.Items) != 1 || listed.Items[0].Name != created.Item.Name {
		t.Fatalf("列表: %#v", listed.Items)
	}

	// ③ 下载（attachment + 内容是 sqlite 文件头）
	response = do(t, handler, http.MethodGet, "/admin/backup/download/"+created.Item.Name, token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("下载 = %d", response.Code)
	}
	if got := response.Header().Get("Content-Disposition"); !strings.Contains(got, created.Item.Name) {
		t.Fatalf("下载该是 attachment: %q", got)
	}
	if !strings.HasPrefix(response.Body.String(), "SQLite format 3") {
		t.Fatalf("下载的该是 sqlite 文件（VACUUM INTO 产物）")
	}

	// ④ 路径穿越被拒（safeName 白名单）
	response = do(t, handler, http.MethodGet, "/admin/backup/download/..%2F..%2Fetc%2Fpasswd", token, "")
	if response.Code == http.StatusOK {
		t.Fatal("路径穿越该被拒")
	}

	// ⑤ 删除
	response = do(t, handler, http.MethodDelete, "/admin/backup/"+created.Item.Name, token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("删除 = %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(backups, created.Item.Name)); !os.IsNotExist(err) {
		t.Fatal("删了之后文件该没了")
	}
}

func decode(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	err := json.Unmarshal(response.Body.Bytes(), target)
	if err != nil {
		t.Fatalf("解析响应: %v (%s)", err, response.Body.String())
	}
}
