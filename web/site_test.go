package web

import (
	"context"
	"log/slog"
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

// newTestSite 建一个临时站点（site.yaml + 空库目录; 表由 Open 自己建）。
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

// writeTypesFile 写 site.yaml（站点目录里必须有它）。
func writeTypesFile(basedir, yaml string) error {
	return os.WriteFile(filepath.Join(basedir, siteFile), []byte(yaml), 0o644)
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

// site.yaml 读不到 / 解析不了 ⇒ 报错（不是起个空类型系统）。
func TestOpenRequiresTypes(t *testing.T) {
	basedir := t.TempDir()
	_, err := Open(basedir)
	if err == nil || !strings.Contains(err.Error(), "site.yaml") {
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

// 默认 SQL 日志**不写 SQL 文本与参数**（可能带登录标识）; 只记错误与慢查询。
func TestQuietSQLLogger(t *testing.T) {
	site := newTestSite(t)
	var lines []string
	// 手动调一次默认 logger, 看它写了什么
	site.db = site.db.SetLogger(quietLogger(testLogger(&lines)))
	_, err := site.Engine().CreateNode(site.db, &core.Node{Type: "article", Fields: core.Fields{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = site.db.Add(`SELECT * FROM nope`).FetchList[map[string]any]()
	if err == nil {
		t.Fatal("该报错")
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "INSERT") || strings.Contains(joined, "SELECT") {
		t.Fatalf("默认日志不该写 SQL 文本: %q", joined)
	}
	if !strings.Contains(joined, "database error") {
		t.Fatalf("错误该记一条: %q", joined)
	}
}

// testLogger 收集 slog 输出（不碰全局 logger）。
func testLogger(lines *[]string) *slog.Logger {
	return slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		*lines = append(*lines, string(p))
		return len(p), nil
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// 站点自己的配置放 site.yaml 的 **fields:** 子树（框架不解释里面的键）。
//
// 分工: 仓库级的 sites.yaml 只回答"哪个目录 + 认哪些域名"（部署拓扑）; 站点自己的
// base_url / oss_bucket / feishu_hooks 这类东西跟着站点走 —— 一个站一个目录, 搬走就是
// 一个目录。
func TestSiteFields(t *testing.T) {
	basedir := t.TempDir()
	err := writeTypesFile(basedir, `
name: 测试站
fields:
  base_url: https://example.com
  oss_bucket: ""
  feishu_hooks:
    contact: https://open.example/hook
types:
  article:
    fields:
      - { name: title, kind: text }
`)
	if err != nil {
		t.Fatal(err)
	}
	site, err := Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = site.Close() })

	if got := site.Field("base_url"); got != "https://example.com" {
		t.Fatalf("base_url = %#v", got)
	}
	fields := site.Fields()
	if fields.Str("oss_bucket") != "" {
		t.Fatalf("oss_bucket 该是空串, 实际 %#v", fields["oss_bucket"])
	}
	// 嵌套的映射解出来是 core.Fields（同一个自由映射类型, 不是 map[string]any）——
	// 站点那边用 yaml 往返把它变成自己的 typed 配置, 所以两种都认。
	hooks := map[string]any{}
	switch typed := fields["feishu_hooks"].(type) {
	case map[string]any:
		hooks = typed
	case core.Fields:
		for key, value := range typed {
			hooks[key] = value
		}
	}
	if hooks["contact"] != "https://open.example/hook" {
		t.Fatalf("嵌套的 feishu_hooks 该原样拿到: %#v", fields["feishu_hooks"])
	}
	// 副本: 调用方改了不影响站点（配置是值, 不是共享可变状态）
	fields["base_url"] = "改了"
	if site.Field("base_url") != "https://example.com" {
		t.Fatal("Fields() 该返回副本")
	}
}

// 没写 fields 也能起（可选）; 但顶层拼错键仍然报错（fields 里面自由, 外面严格）。
func TestSiteFieldsOptionalButTopLevelStrict(t *testing.T) {
	basedir := t.TempDir()
	err := writeTypesFile(basedir, `
name: 测试站
types:
  article:
    fields:
      - { name: title, kind: text }
`)
	if err != nil {
		t.Fatal(err)
	}
	site, err := Open(basedir)
	if err != nil {
		t.Fatalf("没写 fields 该能起: %v", err)
	}
	t.Cleanup(func() { _ = site.Close() })
	if len(site.Fields()) != 0 {
		t.Fatalf("没写 fields 该是空映射: %#v", site.Fields())
	}

	badDir := t.TempDir()
	err = writeTypesFile(badDir, `
name: 测试站
fieldz:
  typo: 1
types:
  article:
    fields:
      - { name: title, kind: text }
`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(badDir)
	if err == nil {
		t.Fatal("顶层拼错键该报错（严格解析只在最外层; fields 里面才自由）")
	}
}
