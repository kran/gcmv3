package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 夹具: 一个只有 staff 类型的最小站点目录 + 一个标记自己是谁的 /ping 路由。
func muxSite(t *testing.T, marker string) string {
	t.Helper()
	basedir := t.TempDir()
	err := os.WriteFile(filepath.Join(basedir, "site.yaml"), []byte(`
types:
  staff:
    capabilities:
      authentication: { roles: [管理员] }
    fields:
      - { name: name, kind: text }
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return basedir
}

func pingMarker(site *Site, marker string) {
	site.Hook(HookBeforeMount, func(s *Site) error {
		s.Router().Get("/ping", func(ctx *CmsCtx) {
			_, _ = ctx.W.Write([]byte(marker))
		})
		return nil
	})
}

// 按 Host 分发; 端口与大小写都不影响匹配。
func TestHostMuxDispatchesByHost(t *testing.T) {
	mux := NewHostMux()
	a, err := mux.Add([]string{"a.test", "www.a.test"}, muxSite(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := mux.Add([]string{"b.test"}, muxSite(t, "b"))
	if err != nil {
		t.Fatal(err)
	}
	pingMarker(a, "站点 A")
	pingMarker(b, "站点 B")
	handler := mux.Start()

	cases := []struct {
		host string
		want string
	}{
		{"a.test", "站点 A"},
		{"A.TEST:8080", "站点 A"}, // 大小写 + 端口都不算差异
		{"www.a.test", "站点 A"},
		{"b.test", "站点 B"},
	}
	for _, one := range cases {
		request := httptest.NewRequest(http.MethodGet, "/ping", nil)
		request.Host = one.host
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || recorder.Body.String() != one.want {
			t.Fatalf("Host %q ⇒ %d %q（想要 %q）", one.host, recorder.Code, recorder.Body.String(), one.want)
		}
	}

	// 未匹配 ⇒ 404（没设兜底）
	request := httptest.NewRequest(http.MethodGet, "/ping", nil)
	request.Host = "nope.test"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未匹配的 Host 该 404, 实际 %d", recorder.Code)
	}

	// 设了兜底 ⇒ 走兜底站点
	mux.SetFallback(a)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "站点 A" {
		t.Fatalf("兜底站点没生效: %d %q", recorder.Code, recorder.Body.String())
	}
}

// 同一个 Host 被两个站抢 ⇒ 当场报错（v2 是静默覆盖, 表现是"谁能访问看运气"）。
func TestHostMuxRejectsDuplicateHost(t *testing.T) {
	mux := NewHostMux()
	_, err := mux.Add([]string{"same.test"}, muxSite(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = mux.Add([]string{"SAME.TEST:80"}, muxSite(t, "b"))
	if err == nil {
		t.Fatal("同一个 Host 注册两次该报错")
	}
}

// 没给可用 Host ⇒ 报错（配错了别让它静默变成一个不接任何域名的站）。
func TestHostMuxRequiresHost(t *testing.T) {
	mux := NewHostMux()
	_, err := mux.Add(nil, muxSite(t, "a"))
	if err == nil {
		t.Fatal("没给 Host 该报错")
	}
}
