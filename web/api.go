// 通用节点 API —— 客户端发起的 CRUD, 每个动作都过策略（D 步的受管入口）。
//
//	GET    /api/nodes/{type}         列表: filter / sort / page / size
//	POST   /api/nodes/{type}         创建      体: {"fields": {…}}
//	GET    /api/nodes/{type}/{id}    单个      按 id 或**地址**（地址是全表唯一的）
//	PUT    /api/nodes/{type}/{id}    更新      体: {"revision": N, "fields": {…}}（乐观锁必填）
//	DELETE /api/nodes/{type}/{id}    删除
//
// 参数:
//
//	filter=lisp 形如 `(and (= $state "published") (in ->categories [1 2]))`
//	sort=字段,-字段        最多 4 个（`-` = 倒序; 可排序性由类型声明决定）
//	page=1&size=20         1 起; size 上限 100
//
// **没有 expand 参数**: 读入口按类型自动展开一层（引用字段里只有 id, 界面没法用）,
// 展开目标各自按自己的读规则掩码。
//
// 两条顺序上的讲究（都在入口里, 不在 handler 里）:
//
//	授权先于存在性检查   未注册写规则的类型不能变成"这个 id 存不存在"的探测器
//	路径上的 type 必须一致  拿另一个类型的路由打同一个 id 得不到东西（含地址）
package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/kran/cho"
	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
)

const (
	defaultPageSize = 20
	maxPageSize     = 100
	maxSortFields   = 4
	maxPageNumber   = 1_000_000 // 页码上限（防 offset 溢出; 真要翻这么多页得先换查询）
	maxJSONBody     = 1 << 20   // 1 MiB（部署层的 client_max_body_size 是另一道）
)

// setupApi 挂通用 API（Setup 的第四步）。
func (s *Site) setupApi() {
	s.router.Group("/api", func(g *cho.Cho[*CmsCtx]) {
		g.Get("/nodes/{type}", s.apiList)
		g.Post("/nodes/{type}", s.apiCreate)
		g.Get("/nodes/{type}/{id}", s.apiView)
		g.Put("/nodes/{type}/{id}", s.apiUpdate)
		g.Delete("/nodes/{type}/{id}", s.apiDelete)
	})
}

// apiList GET /api/nodes/{type}
func (s *Site) apiList(ctx *CmsCtx) {
	typ, err := s.apiType(ctx)
	if err != nil {
		ctx.Fail(err)
		return
	}
	where, err := parseFilter(ctx.Query("filter"))
	if err != nil {
		ctx.Fail(err)
		return
	}
	sort, err := parseSort(ctx.Query("sort"))
	if err != nil {
		ctx.Fail(err)
		return
	}
	page, size := pageParams(ctx)
	items, total, err := ctx.List(core.NodeQuery{Type: typ, Where: where, Sort: sort}, size, (page-1)*size)
	if err != nil {
		ctx.Fail(err)
		return
	}
	_ = ctx.Json(http.StatusOK, map[string]any{
		"items": items, "total": total, "page": page, "size": size,
	})
}

// apiCreate POST /api/nodes/{type}
func (s *Site) apiCreate(ctx *CmsCtx) {
	typ, err := s.apiType(ctx)
	if err != nil {
		ctx.Fail(err)
		return
	}
	var input struct {
		Fields core.Fields `json:"fields"`
	}
	err = ctx.BindJSON(&input)
	if err != nil {
		ctx.Fail(err)
		return
	}
	node, err := ctx.Create(typ, input.Fields)
	if err != nil {
		ctx.Fail(err)
		return
	}
	_ = ctx.Json(http.StatusCreated, map[string]any{"node": node})
}

// apiView GET /api/nodes/{type}/{id}
//
// id 是数字按 id、否则按**地址**（两者都不可能跨类型命中 —— 入口会核对类型）。
func (s *Site) apiView(ctx *CmsCtx) {
	typ, err := s.apiType(ctx)
	if err != nil {
		ctx.Fail(err)
		return
	}
	ref, err := apiRef(ctx)
	if err != nil {
		ctx.Fail(err)
		return
	}
	node, err := ctx.Get(typ, ref)
	if err != nil {
		ctx.Fail(err)
		return
	}
	if node == nil {
		ctx.Fail(NotFound("不存在"))
		return
	}
	_ = ctx.Json(http.StatusOK, map[string]any{"node": node})
}

// apiUpdate PUT /api/nodes/{type}/{id}
func (s *Site) apiUpdate(ctx *CmsCtx) {
	typ, err := s.apiType(ctx)
	if err != nil {
		ctx.Fail(err)
		return
	}
	id, err := apiID(ctx)
	if err != nil {
		ctx.Fail(err)
		return
	}
	// 直接解进 NodePatch: revision / fields 两个字段名就是协议, Type 是 json:"-"
	// （客户端塞不进来）。
	var patch core.NodePatch
	err = ctx.BindJSON(&patch)
	if err != nil {
		ctx.Fail(err)
		return
	}
	node, err := ctx.Update(typ, id, &patch)
	if err != nil {
		ctx.Fail(err)
		return
	}
	_ = ctx.Json(http.StatusOK, map[string]any{"node": node})
}

// apiDelete DELETE /api/nodes/{type}/{id}
func (s *Site) apiDelete(ctx *CmsCtx) {
	typ, err := s.apiType(ctx)
	if err != nil {
		ctx.Fail(err)
		return
	}
	id, err := apiID(ctx)
	if err != nil {
		ctx.Fail(err)
		return
	}
	err = ctx.Delete(typ, id)
	if err != nil {
		ctx.Fail(err)
		return
	}
	_ = ctx.NoContent(http.StatusNoContent)
}

// ── 参数与请求体 ──

// apiType 路径上的类型必须存在（拼错的类型是客户端的事 ⇒ 404, 不是空列表）。
func (s *Site) apiType(c *CmsCtx) (string, error) {
	typ := c.PathValue("type")
	_, ok := s.types.Type(typ)
	if !ok {
		return "", NotFound("类型 %q 不存在", typ)
	}
	return typ, nil
}

// apiRef 路径上的 {id}: 正整数当 id, 否则当**地址**（与引擎 GetNode 同一套定位）。
func apiRef(c *CmsCtx) (any, error) {
	raw := c.PathValue("id")
	if raw == "" {
		return nil, BadRequest("缺少 id")
	}
	if id, ok := parsePositiveID(raw); ok {
		return id, nil
	}
	return raw, nil
}

func apiID(c *CmsCtx) (int64, error) {
	raw := c.PathValue("id")
	id, ok := parsePositiveID(raw)
	if !ok {
		return 0, BadRequest("id 必须是正整数（地址用 GET）")
	}
	return id, nil
}

func parsePositiveID(raw string) (int64, bool) {
	var id int64
	for i := 0; i < len(raw); i++ {
		digit := raw[i]
		if digit < '0' || digit > '9' {
			return 0, false
		}
		id = id*10 + int64(digit-'0')
		if id > 1<<62 {
			return 0, false
		}
	}
	if id <= 0 {
		return 0, false
	}
	return id, true
}

// pageParams 页码与页大小（都夹在合理范围内: 客户端给的垃圾不该变成错误, 也不该
// 变成一次全表扫描）。
func pageParams(c *CmsCtx) (page, size int) {
	page = int(c.QueryNum("page", 1))
	if page < 1 {
		page = 1
	}
	if page > maxPageNumber {
		page = maxPageNumber
	}
	size = int(c.QueryNum("size", defaultPageSize))
	if size < 1 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	return page, size
}

// parseFilter lisp 文本 → 条件。**不在这里校验算符与字段** —— 那是编译期的事
// （core 报 ErrInvalidOperator/ErrInvalidField, 错误出口翻成 400）。
func parseFilter(raw string) (so.Where, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return so.Where{}, nil
	}
	where, err := so.ParseWhere(raw, nil)
	if err != nil {
		return so.Where{}, BadRequest("%s", err.Error())
	}
	return where, nil
}

// parseSort `字段,-字段` → 排序请求。字段名同样留给编译期校验（可排序性来自声明）。
func parseSort(raw string) ([]so.SortField, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > maxSortFields {
		return nil, BadRequest("sort: 最多 %d 个字段", maxSortFields)
	}
	out := make([]so.SortField, 0, len(parts))
	for _, part := range parts {
		field := strings.TrimSpace(part)
		desc := strings.HasPrefix(field, "-")
		field = strings.TrimPrefix(field, "-")
		if field == "" {
			return nil, BadRequest("sort: 字段名为空")
		}
		path, err := so.ParsePath(field)
		if err != nil {
			return nil, BadRequest("sort: %s", err.Error())
		}
		out = append(out, so.SortField{Path: path, Desc: desc})
	}
	return out, nil
}

// BindJSON 解一个 JSON 值到 dst: 拒绝未知字段、拒绝多余值、限制体积。
//
// 严格是有意的: 客户端拼错字段名（`field` vs `fields`）要当场知道, 而不是解析出一个
// 零值然后"看起来成功但什么都没改"。
func (c *CmsCtx) BindJSON(dst any) error {
	body := http.MaxBytesReader(c.W, c.R.Body, maxJSONBody)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(dst)
	if err != nil {
		return jsonBodyError(err)
	}
	var extra any
	err = decoder.Decode(&extra)
	if !errors.Is(err, io.EOF) {
		return BadRequest("请求体只能有一个 JSON 值")
	}
	return nil
}

func jsonBodyError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return Errorf(http.StatusRequestEntityTooLarge, "请求体过大（上限 %d 字节）", maxJSONBody)
	}
	return BadRequest("请求体不合法: %s", err.Error())
}
