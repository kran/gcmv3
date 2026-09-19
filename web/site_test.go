package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
)

const testTypesYAML = `
types:
  article:
    fields:
      - { name: title, kind: text }
`

// newTestSite 建一个临时站点（types.yaml + 空库目录; 表由 Open 自己建）。
func newTestSite(t *testing.T) *Site {
	t.Helper()
	basedir := t.TempDir()
	writeTypes(t, basedir)
	site, err := Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = site.Close() })
	return site
}

// writeTypesFile 写 types.yaml（站点目录里必须有它）。
func writeTypesFile(basedir, yaml string) error {
	return os.WriteFile(filepath.Join(basedir, typesFile), []byte(yaml), 0o644)
}

func writeTypes(t *testing.T, basedir string) {
	t.Helper()
	err := writeTypesFile(basedir, testTypesYAML)
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpen(t *testing.T) {
	site := newTestSite(t)
	if site.Engine() == nil || site.Types() == nil || site.DB() == nil {
		t.Fatal("装配不完整")
	}
	// 引擎真的能用
	id, err := site.Engine().CreateNode(nil, &core.Node{Type: "article", Fields: core.Fields{"title": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	node, err := site.Engine().GetNode(id)
	if err != nil || node.Fields.Str("title") != "甲" {
		t.Fatalf("node = %#v, err = %v", node, err)
	}
	// 档位由 openDB 拼死（不是靠运行时校验）
	var mode string
	err = site.DB().Pool().QueryRow("PRAGMA journal_mode").Scan(&mode)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q（DSN 该拼死 WAL）", mode)
	}
}

// Open 自己建表（基础 schema, 幂等）—— 空目录开出来就能用。
func TestOpenAppliesSchema(t *testing.T) {
	basedir := t.TempDir()
	writeTypes(t, basedir)
	site, err := Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for _, table := range []string{"nodes", "edges", "auth_methods", "sessions"} {
		found, err := site.DB().Add(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = #{1}`, table).FetchOne[string]()
		if err != nil {
			t.Fatal(err)
		}
		if found == nil {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("缺表: %v", missing)
	}
	found, err := site.DB().Add(
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_nodes_address'`).FetchOne[string]()
	if err != nil {
		t.Fatal(err)
	}
	if found == nil {
		t.Fatal("索引也该建出来")
	}
	if err := site.Close(); err != nil {
		t.Fatal(err)
	}

	// 再开一次同一个目录: 幂等（IF NOT EXISTS）, 表还在、不炸
	again, err := Open(basedir)
	if err != nil {
		t.Fatalf("重复 Open 该幂等: %v", err)
	}
	defer again.Close()
	if _, err := again.Engine().CreateNode(nil, &core.Node{Type: "article", Fields: core.Fields{"title": "甲"}}); err != nil {
		t.Fatal(err)
	}
}

// types.yaml 读不到 / 解析不了 ⇒ 报错（不是起个空类型系统）。
func TestOpenRequiresTypes(t *testing.T) {
	basedir := t.TempDir()
	_, err := Open(basedir)
	if err == nil || !strings.Contains(err.Error(), "read types") {
		t.Fatalf("err = %v", err)
	}
}

// CmsCtx.DB() 绑定请求 ctx: 请求取消 ⇒ 查询被取消（不挂库上）。
func TestCmsCtxDBBindsRequestContext(t *testing.T) {
	site := newTestSite(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)

	cms := site.CmsCtxMaker(httptest.NewRecorder(), req)
	_, err := cms.Engine().GetNodes(core.NodeQuery{Type: "article"}, 0, 0)
	if err != nil {
		t.Logf("引擎读（不收句柄, 用自持句柄）: %v", err)
	}
	// 用 ctx 绑定的句柄读 ⇒ 取消要传到 SQL
	_, err = site.DB().WithCtx(cms.R.Context()).Add(`SELECT COUNT(1) FROM nodes`).FetchOne[int64]()
	if err == nil {
		t.Fatal("取消的 ctx 该让查询失败")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err = %v", err)
	}
}

// 响应方法来自 cho（这里只验它们在这个上下文里可用）。
func TestCmsCtxRespond(t *testing.T) {
	site := newTestSite(t)
	recorder := httptest.NewRecorder()
	cms := site.CmsCtxMaker(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	err := cms.Json(http.StatusOK, map[string]any{"ok": true})
	if err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"ok":true`) {
		t.Fatalf("响应 = %d %q", recorder.Code, recorder.Body.String())
	}
	if cms.Types() == nil || cms.Site() != site {
		t.Fatal("上下文没接上站点")
	}
}
