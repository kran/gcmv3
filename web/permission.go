// 权限矩阵 —— 拿**身份样本**去真的跑一遍策略, 把结果摊成一张表。
//
//	GET /admin/permissions?actor=anonymous&actor=member:editor&actor=staff:owner
//
// 这是唯一能发现"策略写错了"的工具。v2 的注释里记着一回: 有人把 OnDelete 写成无条件
// `return nil`, 页面上"删除"那一列对匿名全是允许 —— 才发现"谁都能删"。
//
// 三条设计:
//
//	① **不写第二个真相来源**: 每个格子都是真的调一次规则算出来的（读/建/改/删四个
//	   动词各走平时的求值路径）, 而不是把 types 声明再解释一遍 —— 声明与规则一旦
//	   分叉, 这张表会跟着一起撒谎。
//	② **样本是真实节点**: 改/删两列要拿该类型的一个真节点当样本（规则常常看归属/
//	   状态）, 拿零值节点算出来的结果没有意义。
//	③ **读规则不成立 ⇒ 全列不可读**（fail-closed）: v2 遇到读规则报错时把"隐藏字段"
//	   留空 ⇒ 表格显示"都看得见" ✗ —— 诊断工具在不确定时必须往严里说。
//
// actor 的三种写法: `anonymous`、`<节点类型>`、`<节点类型>:角色,角色`。不给参数时给
// 默认档（匿名 + 每个认证类型 × {无角色, owner}）。角色词表由前端从 /admin/types 取,
// 服务端不预置组合。
package web

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/kran/cho"
	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// PermissionScene 一份身份样本（表格的一列）。
type PermissionScene struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Kind     string   `json:"kind"` // anonymous | node
	NodeType string   `json:"node_type,omitempty"`
	Roles    []string `json:"roles"`
	SampleID int64    `json:"sample_id,omitempty"` // 改/删列用的真实节点
	Note     string   `json:"note,omitempty"`      // 求值前提的说明

	realm string // 内部: SetActor 要渠道名
}

// PermissionRow 一个字段 × 三列（读/建/改）。删除没有字段维度, 单独放在 PermissionType。
type PermissionRow struct {
	Type      string          `json:"type"`
	Label     string          `json:"label"`
	Field     string          `json:"field"`
	FieldName string          `json:"field_name"`
	Kind      string          `json:"kind"`
	Read      map[string]bool `json:"read"`   // 格子 = 这个身份**看得见**吗
	Create    map[string]bool `json:"create"` // 格子 = 这个身份**建得出来**吗
	Update    map[string]bool `json:"update"` // 格子 = 这个身份**改得动**吗
}

// PermissionType 类型级信息（删除是节点级权限, 没有字段维度）。
type PermissionType struct {
	Type     string            `json:"type"`
	Label    string            `json:"label"`
	Delete   map[string]bool   `json:"delete"`
	Scope    map[string]string `json:"scope"` // scene → all / restricted / none（行范围）
	SampleID int64             `json:"sample_id,omitempty"`
}

// adminPermissions GET /admin/permissions
func (s *Site) adminPermissions(ctx *CmsCtx) {
	scenes, err := s.permissionScenes(ctx)
	if err != nil {
		ctx.Fail(BadRequest("%s", err.Error()))
		return
	}
	names := s.types.Names()
	sort.Strings(names)

	typeRows := make([]PermissionType, 0, len(names))
	rows := make([]PermissionRow, 0, 64)
	for _, typeName := range names {
		def, ok := s.types.Type(typeName)
		if !ok {
			continue
		}
		label := def.Admin.Label
		if label == "" {
			label = typeName
		}
		sample := s.sampleNode(typeName)

		// 删除: 节点级（引擎的 restrict 是数据层的事, 与权限无关）
		deletes := map[string]bool{}
		scopes := map[string]string{}
		reads := map[string][]string{}   // sceneID → **看不见**的字段
		creates := map[string][]string{} // sceneID → 建时能写的字段
		updates := map[string][]string{} // sceneID → 改时能写的字段
		for _, scene := range scenes {
			sceneCtx := s.sceneCtx(scene)
			hidden := s.sceneHidden(sceneCtx, typeName)
			reads[scene.ID] = hidden
			creates[scene.ID] = s.sceneWritable(sceneCtx, VerbCreate, typeName, sample, hidden)
			updates[scene.ID] = s.sceneWritable(sceneCtx, VerbUpdate, typeName, sample, hidden)
			deletes[scene.ID] = s.sceneDeletable(sceneCtx, typeName, sample)
			scopes[scene.ID] = s.sceneScope(sceneCtx, typeName)
		}
		typeRows = append(typeRows, PermissionType{
			Type: typeName, Label: label, Delete: deletes, Scope: scopes, SampleID: sampleID(sample),
		})
		for _, field := range def.Fields {
			row := PermissionRow{
				Type: typeName, Label: label,
				Field: field.Name, FieldName: fieldLabel(field), Kind: field.Kind,
				Read: map[string]bool{}, Create: map[string]bool{}, Update: map[string]bool{},
			}
			for _, scene := range scenes {
				row.Read[scene.ID] = !slices.Contains(reads[scene.ID], field.Name)
				row.Create[scene.ID] = slices.Contains(creates[scene.ID], field.Name)
				row.Update[scene.ID] = slices.Contains(updates[scene.ID], field.Name)
			}
			rows = append(rows, row)
		}
	}
	_ = ctx.Json(http.StatusOK, map[string]any{
		"scenes": scenes, "types": typeRows, "rows": rows,
	})
}

// sceneHidden 这个身份在这类型上**看不见**哪些字段。
//
// 读规则不成立（没注册/报错）⇒ 返回**全部字段**（fail-closed: 诊断工具在不确定时
// 必须往严里说, 不能显示成"都看得见"）。
func (s *Site) sceneHidden(c *CmsCtx, typeName string) []string {
	_, hidden, err := c.readRule(typeName)
	if err != nil {
		hidden = []string{}
		if def, ok := s.types.Type(typeName); ok {
			for _, field := range def.Fields {
				hidden = append(hidden, field.Name)
			}
		}
		return hidden
	}
	return hidden
}

// sceneWritable 这个身份在这类型上、这个动作能写哪些字段（真的跑一遍规则）。
func (s *Site) sceneWritable(c *CmsCtx, verb Verb, typeName string, sample *core.Node, hidden []string) []string {
	policy := s.policy(typeName)
	if policy == nil || !policy.registered(verb) {
		return nil
	}
	def, ok := s.types.Type(typeName)
	if !ok {
		return nil
	}
	allow := newWriteGrant()
	var err error
	switch verb {
	case VerbCreate:
		err = policy.OnCreate(c, &core.Node{Type: typeName, Fields: core.Fields{}}, allow)
	case VerbUpdate:
		if sample == nil {
			return nil // 没有真节点当样本 ⇒ 改列按拒绝算（与 v2 同）
		}
		probe := *sample
		err = policy.OnUpdate(c, &probe, &core.NodePatch{}, allow)
	}
	if err != nil {
		return nil
	}
	roles := c.Actor().Roles
	out := make([]string, 0, len(def.Fields))
	for _, field := range def.Fields {
		// 可写 ⊆ 可读（与写入口同一套判断: 藏起来的字段连写也不行）
		if slices.Contains(hidden, field.Name) {
			continue
		}
		if allow.Has(roles, field.Name) {
			out = append(out, field.Name)
		}
	}
	return out
}

// sceneDeletable 这个身份删得掉吗（跑一遍删除规则; 没有样本节点 ⇒ 按拒绝算）。
func (s *Site) sceneDeletable(c *CmsCtx, typeName string, sample *core.Node) bool {
	// registered 对 nil policy 安全 —— 别直接调 policy.OnDelete（没注册时它是个 nil 函数,
	// 调用即崩; 这也是为什么求值前一定要问"注册了吗"）。
	if sample == nil || !s.policy(typeName).registered(VerbDelete) {
		return false
	}
	err := s.policy(typeName).OnDelete(c, sample)
	return err == nil
}

// sceneScope 这个身份在这类型上的**行范围**: all / restricted / none。
//
// read 列只说"字段看不看得见"; 行范围是另一半（也最容易写错）—— 一个把范围写成
// `false` 的读规则, 字段列全绿而实际一行都读不到。
func (s *Site) sceneScope(c *CmsCtx, typeName string) string {
	where, _, err := c.readRule(typeName)
	if err != nil || where.IsZero() {
		return "none" // 读规则不成立 ⇒ 一行都读不到（fail-closed）
	}
	switch constantName(where) {
	case "true":
		return "all"
	case "false":
		return "none"
	}
	return "restricted"
}

// constantName 形状是 so.P("true"/"false") 这种**零参数谓词**时返回它的名字。
func constantName(where so.Where) string {
	tree, ok := where.Tree().([]any)
	if !ok || len(tree) != 1 {
		return ""
	}
	name, _ := tree[0].(string)
	return name
}

// sampleNode 该类型的一个真实节点（改/删列求值用）。
func (s *Site) sampleNode(typeName string) *core.Node {
	nodes, err := s.engine.GetNodes(core.NodeQuery{Type: typeName}, 1, 0)
	if err != nil || len(nodes) == 0 {
		return nil
	}
	return nodes[0]
}

// permissionScenes 解析本次要看哪些身份样本。
func (s *Site) permissionScenes(ctx *CmsCtx) ([]PermissionScene, error) {
	specs := ctx.R.URL.Query()["actor"]
	if len(specs) == 0 {
		return s.defaultScenes(), nil
	}
	out := make([]PermissionScene, 0, len(specs))
	for _, spec := range specs {
		scene, err := s.parseScene(spec)
		if err != nil {
			return nil, err
		}
		out = append(out, scene)
	}
	return out, nil
}

// parseScene `anonymous` 或 `<节点类型>[:角色,角色]` → 一份身份样本。
func (s *Site) parseScene(spec string) (PermissionScene, error) {
	if spec == "anonymous" {
		return PermissionScene{ID: spec, Label: "匿名", Kind: "anonymous"}, nil
	}
	nodeType, roleList, _ := strings.Cut(spec, ":")
	def, ok := s.types.Type(nodeType)
	if !ok {
		return PermissionScene{}, fmt.Errorf("未知的节点类型 %q", nodeType)
	}
	if def.Capabilities.Authentication == nil {
		return PermissionScene{}, fmt.Errorf("类型 %q 不能登录（没有 authentication 能力）", nodeType)
	}
	realm, ok := s.realmForType(nodeType)
	if !ok {
		return PermissionScene{}, fmt.Errorf("类型 %q 没有注册认证渠道", nodeType)
	}
	label := def.Admin.Label
	if label == "" {
		label = nodeType
	}
	scene := PermissionScene{
		ID: spec, Label: label, Kind: "node",
		NodeType: nodeType, realm: realm,
	}
	for _, role := range strings.Split(roleList, ",") {
		if role = strings.TrimSpace(role); role != "" {
			scene.Roles = append(scene.Roles, role)
		}
	}
	if len(scene.Roles) > 0 {
		scene.Label = label + " + " + strings.Join(scene.Roles, "·")
	}
	sample := s.sampleNode(nodeType)
	if sample != nil {
		scene.SampleID = sample.ID
	} else {
		scene.Note = "该类型还没有节点 —— 改/删两列按拒绝算"
	}
	return scene, nil
}

// defaultScenes 不给参数时的默认档。
func (s *Site) defaultScenes() []PermissionScene {
	out := []PermissionScene{{ID: "anonymous", Label: "匿名", Kind: "anonymous"}}
	for _, realm := range s.auth.Realms() {
		def, ok := s.types.Type(realm.NodeType)
		if !ok || def.Capabilities.Authentication == nil {
			continue
		}
		for _, role := range []string{"", types.RoleOwner} {
			spec := realm.NodeType
			if role != "" {
				spec += ":" + role
			}
			scene, err := s.parseScene(spec)
			if err == nil {
				out = append(out, scene)
			}
		}
	}
	return out
}

// realmForType 该节点类型对应的渠道（SetActor 要渠道名）。
func (s *Site) realmForType(nodeType string) (string, bool) {
	for _, realm := range s.auth.Realms() {
		if realm.NodeType == nodeType {
			return realm.Name, true
		}
	}
	return "", false
}

// sceneCtx 一份身份样本的一次性上下文 —— **不带 HTTP 请求**（规则是策略代码, 不该碰
// 响应）; 给它一个丢弃用的响应端只是为了满足 cho 的上下文形状（规则里若真写响应,
// 也不会污染任何东西）。
func (s *Site) sceneCtx(scene PermissionScene) *CmsCtx {
	request, err := http.NewRequest(http.MethodGet, "/admin/permissions", nil)
	if err != nil {
		panic("web: permission scene request: " + err.Error())
	}
	c := &CmsCtx{BaseContext: cho.MakeBaseContext(discardWriter{}, request), site: s}
	if scene.Kind == "anonymous" {
		c.SetActor(Actor{})
		return c
	}
	c.SetActor(Actor{
		NodeID: scene.SampleID, NodeType: scene.NodeType,
		Realm: scene.realm, Roles: scene.Roles,
	})
	return c
}

// discardWriter 占位响应端（矩阵求值不写响应）。
type discardWriter struct{}

func (discardWriter) Header() http.Header         { return http.Header{} }
func (discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (discardWriter) WriteHeader(int)             {}

func sampleID(node *core.Node) int64 {
	if node == nil {
		return 0
	}
	return node.ID
}

func fieldLabel(field types.FieldDef) string {
	if field.Label == "" {
		return field.Name
	}
	return field.Label
}
