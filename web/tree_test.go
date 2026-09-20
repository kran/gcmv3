package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// treeTypesYAML 一棵分类树（admin.view: tree + admin.tree 指父字段）与一个普通类型。
//
//	parent 声明了 transitive: true ⇒ 引擎在写边时就不许成环。
const treeTypesYAML = `
types:
  category:
    admin: { view: tree, tree: parent, label: 分类, group: 基础数据, columns: [name, sort] }
    fields:
      - { name: name, kind: text }
      - { name: sort, kind: number }
      - { name: parent, kind: ref, to: category, transitive: true }
  article:
    fields:
      - { name: title, kind: text }
  staff:
    capabilities:
      authentication: { roles: [editor] }
    fields:
      - { name: name, kind: text }
`

func treeSite(t *testing.T) *Site {
	t.Helper()
	basedir := t.TempDir()
	err := writeTypesFile(basedir, treeTypesYAML)
	if err != nil {
		t.Fatal(err)
	}
	site, err := Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = site.Close() })
	site.Auth().Register(AuthRealm{Name: "staff", NodeType: "staff"})
	openAll := func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	}
	// category 可读可写（建树用）; staff 没声明（用来验"不是树视图"那条）
	site.Type("category").OnRead(openAll)
	site.Type("category").OnCreate(func(_ *CmsCtx, _ *core.Node, allow *Grant) error {
		allow.Add(types.RolePublic, "name", "sort", "parent")
		return nil
	})
	site.Type("article").OnRead(openAll)
	return site
}

// treeCategory 建一个分类（parent 0 = 根）。
func treeCategory(t *testing.T, site *Site, name string, parent int64, sort int64) int64 {
	t.Helper()
	fields := core.Fields{"name": name, "sort": sort}
	if parent > 0 {
		fields["parent"] = parent
	}
	id, err := site.Engine().CreateNode(nil, &core.Node{Type: "category", Fields: fields})
	if err != nil {
		t.Fatalf("建分类 %q: %v", name, err)
	}
	return id
}

// treeFetch 取一次树。
func treeFetch(t *testing.T, site *Site, target, token string) (treePayload, *http.Response) {
	t.Helper()
	got := do(t, site, http.MethodGet, target, withBearer(token))
	var payload treePayload
	if got.Code == http.StatusOK {
		err := json.Unmarshal(got.Body.Bytes(), &payload)
		if err != nil {
			t.Fatalf("%v: %q", err, got.Body.String())
		}
	}
	return payload, got.Result()
}

type treePayload struct {
	Items []struct {
		ID     int64  `json:"id"`
		Type   string `json:"type"`
		Fields struct {
			Name string `json:"name"`
		} `json:"fields"`
		Children []struct {
			ID     int64 `json:"id"`
			Fields struct {
				Name string `json:"name"`
			} `json:"fields"`
			Children []struct {
				ID     int64 `json:"id"`
				Fields struct {
					Name string `json:"name"`
				} `json:"fields"`
			} `json:"children"`
		} `json:"children"`
	} `json:"items"`
	Total int64 `json:"total"`
}

// 树按 parent 装好, 同级顺序 = 排序顺序(或默认序), 孤儿子节点当根。
func TestAdminTree(t *testing.T) {
	site := treeSite(t)
	root := treeCategory(t, site, "根", 0, 1)
	childA := treeCategory(t, site, "子A", root, 2)
	childB := treeCategory(t, site, "子B", root, 1)
	grand := treeCategory(t, site, "孙", childA, 1)

	owner, _ := adminToken(t, site, types.RoleOwner)
	payload, _ := treeFetch(t, site, "/admin/tree/category?sort=$sort", owner)
	if len(payload.Items) != 1 || payload.Items[0].ID != root {
		t.Fatalf("该只有一个根: %#v", payload.Items)
	}
	root1 := payload.Items[0]
	if root1.Type != "category" || root1.Fields.Name != "根" {
		t.Fatalf("节点字段该是平的（内嵌 core.Node）: %#v", root1)
	}
	// 同级按 sort 升序: 子B(1) 在 子A(2) 前
	if len(root1.Children) != 2 || root1.Children[0].ID != childB || root1.Children[1].ID != childA {
		t.Fatalf("同级顺序该按 sort: %#v", root1.Children)
	}
	// 孙节点在子A下
	if len(root1.Children[1].Children) != 1 || root1.Children[1].Children[0].ID != grand {
		t.Fatalf("孙节点该挂在子A下: %#v", root1.Children[1])
	}
	if payload.Total != 4 {
		t.Fatalf("total = %d", payload.Total)
	}

}

// 读规则挡住的行不进树（树走受管读入口）; 父节点读不到时子节点当根 —— 不静默丢节点。
func TestAdminTreeRespectsReadPolicy(t *testing.T) {
	site := treeSite(t)
	visible := treeCategory(t, site, "看得见", 0, 1)
	hiddenRoot := treeCategory(t, site, "看不见的根", 0, 2)
	hiddenChild := treeCategory(t, site, "父看不到但我看得到", hiddenRoot, 1)
	// 读规则: owner 全部; 别的身份**看不到那个根, 但看得到它的子节点**
	site.Type("category").OnRead(func(c *CmsCtx, where *so.Where, _ *Grant) error {
		if c.Actor().IsOwner() {
			*where = so.P("true")
			return nil
		}
		*where = so.P("!=", "id", hiddenRoot)
		return nil
	})
	owner, _ := adminToken(t, site, types.RoleOwner)
	payload, _ := treeFetch(t, site, "/admin/tree/category", owner)
	if len(payload.Items) != 2 {
		t.Fatalf("owner 不受限, 该看到两棵: %#v", payload.Items)
	}

	// 没有 owner 角色的后台身份（走受限分支）
	admin, _ := adminToken(t, site, types.RoleAdmin)
	payload, _ = treeFetch(t, site, "/admin/tree/category", admin)
	roots := map[int64]bool{}
	for _, item := range payload.Items {
		roots[item.ID] = true
		if item.ID == hiddenRoot {
			t.Fatalf("读不到的行不该进树: %#v", item)
		}
	}
	if !roots[visible] {
		t.Fatalf("看得见的那棵该在: %#v", payload.Items)
	}
	// 父节点读不到 ⇒ 子节点当根（而不是从树上消失）
	if !roots[hiddenChild] {
		t.Fatalf("父节点读不到时子节点该当根: %#v", payload.Items)
	}
	if payload.Total != 2 { // 读得到的是 2 行（根被规则挡住）
		t.Fatalf("total 该是行数（不受树结构影响）: %d", payload.Total)
	}
}

// 配置错与边界: 不是树视图 / 类型不存在 / 有环 ⇒ 明确报错。
func TestAdminTreeErrors(t *testing.T) {
	site := treeSite(t)
	owner, _ := adminToken(t, site, types.RoleOwner)
	// 不是树视图的类型
	got := do(t, site, http.MethodGet, "/admin/tree/article", withBearer(owner))
	if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "不是树视图") {
		t.Fatalf("普通类型 = %d %q", got.Code, got.Body.String())
	}
	// 类型不存在
	got = do(t, site, http.MethodGet, "/admin/tree/nope", withBearer(owner))
	if got.Code != http.StatusNotFound {
		t.Fatalf("未知类型 = %d", got.Code)
	}
	// 排序字段拼错 ⇒ 400
	got = do(t, site, http.MethodGet, "/admin/tree/category?sort=$nope", withBearer(owner))
	if got.Code != http.StatusBadRequest {
		t.Fatalf("排序字段错 = %d", got.Code)
	}
	// 门: 匿名 401
	if got := do(t, site, http.MethodGet, "/admin/tree/category"); got.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 = %d", got.Code)
	}
}

// 环: 报错并打印链路（transitive 的提示写进消息里）。
func TestAdminTreeCycle(t *testing.T) {
	site := treeSite(t)
	a := treeCategory(t, site, "A", 0, 1)
	b := treeCategory(t, site, "B", a, 1)
	// 直接改库造一个环（绕过 transitive 的写边检查）—— 模拟"没声明 transitive"的站点
	// B.parent = A 已经在了（引擎写的）; 再直插一条 A.parent = B ⇒ 环。
	// 走引擎写不出来（transitive: true 会拦住）—— 这里模拟"没声明 transitive 的站点"。
	_, err := site.DB().Insert("edges", map[string]any{
		"from_node": a, "field": "parent", "to_node": b, "sort": 0, "created_at": int64(1),
	}).Exec()
	if err != nil {
		t.Fatal(err)
	}
	owner, _ := adminToken(t, site, types.RoleOwner)
	got := do(t, site, http.MethodGet, "/admin/tree/category", withBearer(owner))
	if got.Code != http.StatusBadRequest {
		t.Fatalf("成环该报错（不是静默丢节点）: %d %q", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), "环") || !strings.Contains(got.Body.String(), "transitive") {
		t.Fatalf("该说清是环 + 怎么根治: %q", got.Body.String())
	}
}

// 上限: 超了明确报错, 不截断。
func TestAdminTreeTooManyNodes(t *testing.T) {
	site := treeSite(t)
	// 用批量插入造 maxTreeNodes+1 个节点（走引擎太慢）
	now := int64(1)
	for i := 0; i <= maxTreeNodes; i++ {
		_, err := site.DB().Insert("nodes", map[string]any{
			"type": "category", "revision": 1, "fields": `{"name":"n"}`, "created_at": now, "updated_at": now,
		}).Exec()
		if err != nil {
			t.Fatal(err)
		}
	}
	owner, _ := adminToken(t, site, types.RoleOwner)
	got := do(t, site, http.MethodGet, "/admin/tree/category", withBearer(owner))
	if got.Code != http.StatusUnprocessableEntity || !strings.Contains(got.Body.String(), "太多") {
		t.Fatalf("超上限该明确报错: %d %q", got.Code, got.Body.String())
	}
}
