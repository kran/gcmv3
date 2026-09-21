// 凭据管理（admin 组里的 owner-only 端点）。
//
// 为什么需要它: 凭据只有"自助注册 / 本人绑定 / 站点 CLI 直连内核"三个入口 ——
// 会员忘了密码只能重新注册, 运维只能手工改库。这里补上"管理员给人设/重置/解绑"。
//
// **只有 owner**（与 roles 同级: 能改别人密码 = 能冒充别人）。admin 角色只给
// "进后台"这道门, 没有字段权限, 也不该能凭据别人的账号。
//
// 两个约定:
//
//	设置凭据只写 {"password": bcrypt(secret)} —— 所以它天然只对**口令类**方式有效
//	（authentication.methods 里声明的那些）; 微信这种外部机制的凭据只能列出与解绑。
//	改完**踢掉该节点所有会话** —— 否则"改密码把坏人踢下线"是句空话。
package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/kran/gcmv3/core"
)

// adminAuthList GET /admin/auth/{type}/{id} → {methods:[{method, identifier, created_at}]}
func (s *Site) adminAuthList(ctx *CmsCtx) {
	if !s.requireOwner(ctx) {
		return
	}
	typeName := strings.TrimSpace(ctx.PathValue("type"))
	nodeID, ok := s.authTargetNode(ctx, typeName)
	if !ok {
		return
	}
	rows, err := s.engine.AuthMethodsOf(typeName, nodeID)
	if err != nil {
		ctx.Fail(CoreError(err))
		return
	}
	// 只下发方法/标识/时间 —— AuthMethod.Data 是 json:"-", 这里再显式构造一次
	// 视图: 哪天有人在 core 那边改了 tag, 也不至于把凭据字段漏出去。
	methods := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		methods = append(methods, map[string]any{
			"method": row.Method, "identifier": row.Identifier, "created_at": row.CreatedAt,
		})
	}
	// 声明过的口令方式也一起给: 前端要拿它做"设置口令"的选项（含那些还没绑的）
	declared, _ := s.types.AuthMethods(typeName)
	_ = ctx.Json(http.StatusOK, map[string]any{"methods": methods, "password_methods": declared})
}

// adminAuthSet POST /admin/auth/{type}/{id}  体: {"method","identifier","secret"}
//
// 已有这条 (方式, 标识) ⇒ 覆盖口令; 没有 ⇒ 新建一条。
func (s *Site) adminAuthSet(ctx *CmsCtx) {
	if !s.requireOwner(ctx) {
		return
	}
	typeName := strings.TrimSpace(ctx.PathValue("type"))
	nodeID, ok := s.authTargetNode(ctx, typeName)
	if !ok {
		return
	}
	var input struct {
		Method     string `json:"method"`
		Identifier string `json:"identifier"`
		Secret     string `json:"secret"`
	}
	err := ctx.BindJSON(&input)
	if err != nil {
		ctx.Fail(err)
		return
	}
	if input.Method == "" || input.Identifier == "" {
		ctx.Fail(BadRequest("method / identifier 必填"))
		return
	}
	// 只允许声明过的**口令类**方式: 否则就是把一条不该走口令核验的凭据写成口令
	//（比如 method=wechat —— 那条本该只认 openid）。
	if !s.types.HasAuthMethod(typeName, input.Method) {
		ctx.Fail(BadRequest("类型 %q 不支持口令方式 %q（见 site.yaml 的 authentication.methods）",
			typeName, input.Method))
		return
	}
	err = checkPassword(input.Secret)
	if err != nil {
		ctx.Fail(err)
		return
	}
	hash, err := hashPassword(input.Secret)
	if err != nil {
		ctx.Fail(err)
		return
	}
	err = s.engine.SetAuthMethod(ctx.DB(), typeName, nodeID, input.Method, input.Identifier,
		core.Fields{passwordKey: hash})
	if err != nil {
		ctx.Fail(CoreError(err))
		return
	}
	s.kickSessions(ctx, nodeID)
	slog.Info("admin: 设置凭据", "type", typeName, "node", nodeID,
		"method", input.Method, "identifier", input.Identifier)
	_ = ctx.NoContent(http.StatusNoContent)
}

// adminAuthRemove DELETE /admin/auth/{type}/{id}/{method}
//
// 解绑该节点在某个方式下的全部凭据（微信这种外部机制的唯一操作）。
func (s *Site) adminAuthRemove(ctx *CmsCtx) {
	if !s.requireOwner(ctx) {
		return
	}
	typeName := strings.TrimSpace(ctx.PathValue("type"))
	nodeID, ok := s.authTargetNode(ctx, typeName)
	if !ok {
		return
	}
	method := strings.TrimSpace(ctx.PathValue("method"))
	if method == "" {
		ctx.Fail(BadRequest("method 必填"))
		return
	}
	// 标识**必填**。同一个方式在正常路径下只会有一条（SetAuthMethod 按方式覆盖）, 但
	// 历史数据里可能有多条 —— 只按 method 删会一次删掉好几条（实测踩过: 连删两条,
	// 只在最后一条被“最后凭据”守卫拦住）。所以只删“界面上看到的那一条”。
	identifier := strings.TrimSpace(ctx.Query("identifier"))
	if identifier == "" {
		ctx.Fail(BadRequest("identifier 必填（同一个方式可能有多条历史凭据, 按方式删会连删）"))
		return
	}
	rows, err := s.engine.AuthMethodsOf(typeName, nodeID)
	if err != nil {
		ctx.Fail(CoreError(err))
		return
	}
	found := false
	for _, row := range rows {
		if row.Method == method && row.Identifier == identifier {
			found = true
			break
		}
	}
	if !found {
		ctx.Fail(NotFound("该节点没有 %q 的凭据 %q", method, identifier))
		return
	}
	err = s.engine.RemoveAuthMethod(ctx.DB(), typeName, method, identifier)
	if errors.Is(err, core.ErrLastAuthMethod) {
		// 业务规则, 不是故障: 说清楚为什么拒（4xx, 别回 500）
		ctx.Fail(BadRequest("这是该账号最后一条登录凭据 —— 删掉他就再也登不进来了" +
			"（先加一条新的再删这条, 或直接停用这个账号）"))
		return
	}
	if err != nil {
		ctx.Fail(CoreError(err))
		return
	}
	s.kickSessions(ctx, nodeID)
	slog.Info("admin: 解绑凭据", "type", typeName, "node", nodeID, "method", method, "identifier", identifier)
	_ = ctx.NoContent(http.StatusNoContent)
}

// requireOwner 凭据管理这类"能冒充别人"的操作只给 owner。
func (s *Site) requireOwner(ctx *CmsCtx) bool {
	if ctx.Actor().IsOwner() {
		return true
	}
	ctx.Fail(Forbidden("只有 owner 能管理登录凭据"))
	return false
}

// authTargetNode 路径上的 {type}/{id} → 节点 id（类型必须是能登录的类型）。
func (s *Site) authTargetNode(ctx *CmsCtx, typeName string) (int64, bool) {
	if typeName == "" {
		ctx.Fail(BadRequest("type 必填"))
		return 0, false
	}
	if _, ok := s.types.AuthMethods(typeName); !ok {
		ctx.Fail(BadRequest("类型 %q 不能登录（没有 authentication 能力）", typeName))
		return 0, false
	}
	raw := strings.TrimSpace(ctx.PathValue("id"))
	nodeID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || nodeID <= 0 {
		ctx.Fail(BadRequest("id 必须是正整数"))
		return 0, false
	}
	// 节点必须存在且类型对得上（否则就是往一个不存在的人身上挂凭据）
	node, err := s.engine.GetNode(nodeID)
	if err != nil || node == nil || node.Type != typeName {
		ctx.Fail(NotFound("节点不存在"))
		return 0, false
	}
	return nodeID, true
}

// kickSessions 改完凭据踢掉该节点所有会话（"改密码把坏人踢下线"）。
func (s *Site) kickSessions(ctx *CmsCtx, nodeID int64) {
	err := s.engine.DeleteNodeSessions(ctx.DB(), nodeID)
	if err != nil {
		// 踢会话失败不该让"已经改好的密码"回到错误状态: 记下来, 管理员再操作一次即可
		slog.Error("admin: 踢会话失败", "node", nodeID, "err", err.Error())
	}
}
