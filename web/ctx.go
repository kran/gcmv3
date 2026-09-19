package web

import (
	"net/http"

	"github.com/kran/cho"
	"github.com/kran/dba"
	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/types"
)

// CmsCtx 一个请求的上下文: 响应方法 + 引擎访问 + （后面的步骤里）当前身份。
//
// 请求/响应那一套由 cho 的 BaseContext 提供（Json / String / Error / BindJson /
// cookie / QueryNum …), 这里只补"这个站点、这个引擎、这个身份"。
//
// 一个请求一个 CmsCtx, **不跨请求复用、不跨 goroutine 共享** —— 后面几步会往里
// 放懒加载的 Actor 与策略求值缓存, 那些都是有状态的。
type CmsCtx struct {
	*cho.BaseContext
	site *Site

	// 身份（懒解析一次 → 缓存; SetActor 覆盖）。
	actor           Actor
	actorLoaded     bool
	principal       *core.Node
	principalLoaded bool

	// 读规则求值结果（每类型一次; 换身份时作废）。
	readRules map[string]readRuleResult
}

// CmsCtxMaker cho 的上下文工厂（Start 建 router 时用）。
func (s *Site) CmsCtxMaker(w http.ResponseWriter, r *http.Request) *CmsCtx {
	return &CmsCtx{BaseContext: cho.MakeBaseContext(w, r), site: s}
}

// Site 本请求所属站点。
func (c *CmsCtx) Site() *Site { return c.site }

// Engine 引擎（handler 里查数据）。
func (c *CmsCtx) Engine() core.Engine { return c.site.engine }

// DB 引擎方法的第一个参数: 绑定本请求 ctx —— 客户端断开即取消查询。
// 想在调用方事务里跑就传事务里的那个句柄（引擎方法的约定）。
func (c *CmsCtx) DB() *dba.SQL { return c.site.handle(c.R.Context()) }

// Types 类型系统（读字段声明/能力时用）。
func (c *CmsCtx) Types() *types.Types { return c.site.types }
