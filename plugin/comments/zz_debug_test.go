package comments

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kran/gcmv3/core"
)

func TestZZDebugAnonymousRead(t *testing.T) {
	h := newHarness(t, Options{})
	article := h.create(t, "article", core.Fields{"name": "动态"})
	for _, tc := range []struct{ name, token string }{{"anon", ""}, {"author", h.token}} {
		request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/nodes/article/%d", article), nil)
		if tc.token != "" {
			request.Header.Set("Authorization", "Bearer "+tc.token)
		}
		recorder := httptest.NewRecorder()
		h.handler.ServeHTTP(recorder, request)
		t.Logf("节点读 %s → %d %s", tc.name, recorder.Code, recorder.Body.String())
	}
	response := h.post(t, map[string]any{"type": "article", "target": article, "body": "第一条"}, h.token)
	t.Logf("发表 → %d %s", response.Code, response.Body.String())
	recorder, _ := h.list(t, map[string]string{"type": "article", "target": fmt.Sprint(article)}, h.token)
	t.Logf("作者列表 → %d %s", recorder.Code, recorder.Body.String())
	recorder, _ = h.list(t, map[string]string{"type": "article", "target": fmt.Sprint(article)}, "")
	t.Logf("匿名列表 → %d %s", recorder.Code, recorder.Body.String())
}
