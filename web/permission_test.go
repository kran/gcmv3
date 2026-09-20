package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// matrixSite 一个有"匿名 / 会员 / 员工"三档身份的站点 —— 正是权限矩阵要看的东西。
//
//	article: 匿名只看已发布且看不到 phone; 作者能改 title, 草稿期还能改 state; owner 全放行
//	member : 只有管理角色看得到（managementOnly 那类）
func matrixSite(t *testing.T) *Site {
	t.Helper()
	site := newPolicySite(t)
	site.Auth().Register(AuthRealm{Name: "staff", NodeType: "staff"})
	site.Type("article").OnRead(func(c *CmsCtx, where *so.Where, hide *Grant) error {
		if c.Actor().IsOwner() {
			*where = so.P("true")
			return nil
		}
		if c.Actor().IsAnonymous() {
			hide.Add(types.RolePublic, "phone")
			*where = so.P("=", "$state", "published")
			return nil
		}
		*where = so.OR(so.P("=", "$state", "published"),
			so.P("ref", "->author", so.P("=", "id", c.Actor().NodeID)))
		return nil
	})
	site.Type("article").OnCreate(func(c *CmsCtx, node *core.Node, allow *Grant) error {
		if c.Actor().IsAnonymous() {
			return Unauthorized("请先登录")
		}
		allow.Add(types.RolePublic, "title", "state")
		node.Fields["author"] = c.Actor().NodeID
		return nil
	})
	site.Type("article").OnUpdate(func(c *CmsCtx, node *core.Node, _ *core.NodePatch, allow *Grant) error {
		if c.Actor().IsOwner() {
			allow.Add(types.RolePublic, "*")
			return nil
		}
		author, _ := node.Fields["author"].(int64)
		if author != c.Actor().NodeID {
			return Forbidden("不是作者")
		}
		allow.Add(types.RolePublic, "title")
		if node.Fields.Str("state") != "published" {
			allow.Add(types.RolePublic, "state")
		}
		return nil
	})
	site.Type("article").OnDelete(func(c *CmsCtx, node *core.Node) error {
		author, _ := node.Fields["author"].(int64)
		if c.Actor().IsOwner() || author == c.Actor().NodeID {
			return nil
		}
		return Forbidden("仅作者或 owner")
	})
	// member: 只有管理角色看得见（也钉住"读规则报错 ⇒ 全列不可读"的反面）
	site.Type("member").OnRead(func(c *CmsCtx, where *so.Where, _ *Grant) error {
		if c.Actor().IsOwner() || c.Actor().HasRole(types.RoleAdmin) {
			*where = so.P("true")
			return nil
		}
		*where = so.P("false")
		return nil
	})
	return site
}

// matrixFetch 取一次矩阵。
func matrixFetch(t *testing.T, site *Site, actors ...string) matrixPayload {
	t.Helper()
	token, _ := adminToken(t, site, types.RoleOwner)
	target := "/admin/permissions"
	if len(actors) > 0 {
		params := url.Values{}
		for _, actor := range actors {
			params.Add("actor", actor)
		}
		target += "?" + params.Encode()
	}
	got := do(t, site, http.MethodGet, target, withBearer(token))
	if got.Code != http.StatusOK {
		t.Fatalf("矩阵 = %d %q", got.Code, got.Body.String())
	}
	var payload matrixPayload
	err := json.Unmarshal(got.Body.Bytes(), &payload)
	if err != nil {
		t.Fatalf("%v: %q", err, got.Body.String())
	}
	return payload
}

type matrixPayload struct {
	Scenes []PermissionScene `json:"scenes"`
	Types  []PermissionType  `json:"types"`
	Rows   []PermissionRow   `json:"rows"`
}

func (p matrixPayload) scene(id string) PermissionScene {
	for _, scene := range p.Scenes {
		if scene.ID == id {
			return scene
		}
	}
	return PermissionScene{}
}

func (p matrixPayload) row(typeName, field string) PermissionRow {
	for _, row := range p.Rows {
		if row.Type == typeName && row.Field == field {
			return row
		}
	}
	return PermissionRow{}
}

func (p matrixPayload) typeRow(typeName string) PermissionType {
	for _, item := range p.Types {
		if item.Type == typeName {
			return item
		}
	}
	return PermissionType{}
}

// 匿名列: 读规则裁掉的字段显示"看不见", 没被裁的显示"看得见"。
func TestPermissionMatrixReadColumn(t *testing.T) {
	site := matrixSite(t)
	payload := matrixFetch(t, site, "anonymous")
	if len(payload.Scenes) != 1 || payload.Scenes[0].ID != "anonymous" {
		t.Fatalf("档 = %#v", payload.Scenes)
	}
	phone := payload.row("article", "phone")
	if phone.Read["anonymous"] {
		t.Fatal("匿名不该看得见 phone")
	}
	if !payload.row("article", "title").Read["anonymous"] {
		t.Fatal("匿名该看得见 title")
	}
	// member 类型: 匿名**字段可见**但**行范围为空** —— 两件事分开看（行范围见 scope）
	if payload.typeRow("member").Scope["anonymous"] != "none" {
		t.Fatalf("匿名对 member 的行范围该是 none: %q", payload.typeRow("member").Scope["anonymous"])
	}
	if payload.typeRow("article").Scope["anonymous"] != "restricted" {
		t.Fatalf("匿名对 article 的行范围该是 restricted: %q", payload.typeRow("article").Scope["anonymous"])
	}
}

// 同一类型的三个档位（匿名 / 会员 / 会员+owner）: 每列都真跑规则算出来。
func TestPermissionMatrixScenes(t *testing.T) {
	site := matrixSite(t)
	// 造一个真实的 article 当样本（改/删列需要）
	id, err := site.Engine().RegisterAuth(nil, "member", "email", "a@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = site.Engine().CreateNode(nil, &core.Node{Type: "article",
		Fields: core.Fields{"title": "草稿", "author": id, "state": "draft"}})
	if err != nil {
		t.Fatal(err)
	}
	payload := matrixFetch(t, site, "anonymous", "member", "member:"+types.RoleOwner)

	anon := payload.scene("anonymous")
	member := payload.scene("member")
	owner := payload.scene("member:" + types.RoleOwner)
	for _, scene := range []PermissionScene{anon, member, owner} {
		if scene.ID == "" {
			t.Fatalf("缺档位: %#v", payload.Scenes)
		}
	}
	if member.SampleID == 0 || owner.SampleID == 0 {
		t.Fatalf("节点档该带样本节点: member=%d owner=%d", member.SampleID, owner.SampleID)
	}
	// 建: 匿名建不了, 会员能建 title/state, owner 能建全部声明字段
	if payload.row("article", "title").Create["anonymous"] {
		t.Fatal("匿名不该能建")
	}
	if !payload.row("article", "title").Create["member"] || !payload.row("article", "state").Create["member"] {
		t.Fatal("会员该能建 title/state")
	}
	if !payload.row("article", "author").Create["member:"+types.RoleOwner] {
		t.Fatal("owner 该能建任意字段（授予了 *）")
	}
	// 改: 作者本人能改 title; 草稿期还能改 state; 匿名改不了
	row := payload.row("article", "title")
	if row.Update["anonymous"] || !row.Update["member"] {
		t.Fatalf("改列: %#v", row.Update)
	}
	if !payload.row("article", "state").Update["member"] {
		t.Fatal("草稿期作者该能改 state")
	}
	// 删: 作者与 owner 能删, 匿名不能
	if payload.typeRow("article").Delete["anonymous"] {
		t.Fatal("匿名不该能删")
	}
	if !payload.typeRow("article").Delete["member"] || !payload.typeRow("article").Delete["member:"+types.RoleOwner] {
		t.Fatal("作者与 owner 该能删")
	}
	// 行范围（read 列只说字段看不看得见, 行范围是另一半）:
	// member 类型规则给 false ⇒ 匿名与普通会员一行都读不到, owner 全部; article 是受限范围
	memberScope := payload.typeRow("member").Scope
	if memberScope["anonymous"] != "none" || memberScope["member"] != "none" {
		t.Fatalf("member 类型该只有管理角色读得到: %#v", memberScope)
	}
	if memberScope["member:"+types.RoleOwner] != "all" {
		// owner 走的是 IsOwner() 分支 ⇒ 范围是 true
		t.Fatalf("会员带 owner 角色该读全部: %#v", memberScope)
	}
	if payload.typeRow("article").Scope["member"] != "restricted" {
		t.Fatalf("会员对 article 该是受限范围: %#v", payload.typeRow("article").Scope)
	}
	if payload.typeRow("article").Scope["member:"+types.RoleOwner] != "all" {
		t.Fatalf("owner 对 article 该是全部: %#v", payload.typeRow("article").Scope)
	}
}

// **fail-closed**: 读规则没注册（或报错）⇒ 全列不可读, 而不是显示成都看得见。
func TestPermissionMatrixReadRuleMissing(t *testing.T) {
	site := matrixSite(t)
	// staff 没有读规则（矩阵站点里只给 article/member 注册了）
	payload := matrixFetch(t, site, "anonymous")
	row := payload.row("staff", "name")
	if row.Field == "" {
		t.Fatalf("没有 staff.name 行: %#v", payload.Rows)
	}
	if row.Read["anonymous"] {
		t.Fatal("读规则不成立时该显示不可读（fail-closed）")
	}
}

// 该类型还没有节点 ⇒ 改/删按拒绝算, 并给出说明。
func TestPermissionMatrixNoSampleNode(t *testing.T) {
	site := matrixSite(t)
	payload := matrixFetch(t, site, "member:"+types.RoleOwner)
	scene := payload.scene("member:" + types.RoleOwner)
	if scene.SampleID != 0 {
		t.Fatalf("还没有节点时不该有样本 id: %d", scene.SampleID)
	}
	if scene.Note == "" || !strings.Contains(scene.Note, "节点") {
		t.Fatalf("该说明为什么改/删是拒绝: %q", scene.Note)
	}
	if payload.row("article", "title").Update["member:"+types.RoleOwner] {
		t.Fatal("没有样本节点时改列该是拒绝")
	}
	if payload.typeRow("article").Delete["member:"+types.RoleOwner] {
		t.Fatal("没有样本节点时删列该是拒绝")
	}
}

// 不给参数时的默认档: 匿名 + 每个认证类型 × {无角色, owner}。
func TestPermissionMatrixDefaultScenes(t *testing.T) {
	site := matrixSite(t)
	payload := matrixFetch(t, site)
	ids := make([]string, 0, len(payload.Scenes))
	for _, scene := range payload.Scenes {
		ids = append(ids, scene.ID)
	}
	joined := strings.Join(ids, ",")
	for _, want := range []string{"anonymous", "member", "member:" + types.RoleOwner, "staff", "staff:" + types.RoleOwner} {
		if !strings.Contains(joined, want) {
			t.Fatalf("默认档缺 %s: %s", want, joined)
		}
	}
}

// 参数错误: 类型不存在 / 类型不能登录 / 类型没有渠道 ⇒ 400（不是 500, 也不是空表）。
func TestPermissionMatrixBadActor(t *testing.T) {
	site := matrixSite(t)
	token, _ := adminToken(t, site, types.RoleAdmin)
	cases := map[string]string{
		"nope":    "未知的节点类型",
		"article": "不能登录",
	}
	for spec, want := range cases {
		got := do(t, site, http.MethodGet, "/admin/permissions?actor="+spec, withBearer(token))
		if got.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %q", spec, got.Code, got.Body.String())
		}
		if !strings.Contains(got.Body.String(), want) {
			t.Fatalf("%s 的错误该说清原因（含 %q）: %q", spec, want, got.Body.String())
		}
	}
}

// 矩阵是后台端点: 匿名 401、无后台角色 403。
func TestPermissionMatrixNeedsAdmin(t *testing.T) {
	site := matrixSite(t)
	if got := do(t, site, http.MethodGet, "/admin/permissions"); got.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 = %d", got.Code)
	}
	token, _ := adminToken(t, site) // 没有 owner/admin 角色
	if got := do(t, site, http.MethodGet, "/admin/permissions", withBearer(token)); got.Code != http.StatusForbidden {
		t.Fatalf("无后台角色 = %d", got.Code)
	}
}

// 矩阵的求值**不改数据**: 跑完一遍, 节点数与字段都没变。
func TestPermissionMatrixIsReadOnly(t *testing.T) {
	site := matrixSite(t)
	id, err := site.Engine().RegisterAuth(nil, "member", "email", "a@x.com",
		core.Fields{}, &core.Node{Fields: core.Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	nodeID, err := site.Engine().CreateNode(nil, &core.Node{Type: "article",
		Fields: core.Fields{"title": "草稿", "author": id, "state": "draft"}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := site.Engine().GetNode(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	matrixFetch(t, site, "anonymous", "member", "member:"+types.RoleOwner)
	after, err := site.Engine().GetNode(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Revision != after.Revision || after.Fields.Str("state") != "draft" {
		t.Fatalf("矩阵求值不该动数据: %d → %d, %#v", before.Revision, after.Revision, after.Fields)
	}
}
