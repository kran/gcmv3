// 节点读路径。
//
// 读 = **取行 + 读投影**。取行就是一条 SELECT; 投影把引用 id 补进 Fields ——
// 于是调用方拿到的 Fields 一律完整（"读出来的值能原样写回"）。
//
// **没有行范围这个参数**: 范围是授权词汇, 归读层 —— 它把规则与客户端条件
// AND 成一个 Where 再传进来（客户端的东西只能当子项 ⇒ 只能更窄）。
// core 这边 `Where == nil` 就是"不过滤"（内部调用方直说"我要全部"）。
package core

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kran/dba"
	gql "github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// NodeQuery 描述"选哪些节点": 类型 + 条件 + 排序 + 展开哪些引用。
//
// 不含分页（在调用参数上）。
type NodeQuery struct {
	Type string
	// Where 是**前端树**（`gql.P/AND/OR/NOT/REF` 的产物）—— core 负责把它读成
	// AST 再编译。零值 = 不过滤。
	Where gql.Where
	Sort  []gql.SortField
	// Expand 展开哪些引用路径（`->field` 或 `*` = 该类型的所有引用字段）。
	//
	// **nil = 全展开**（`*`, 读入口的历史行为, 所有老调用方不变）;
	// **非 nil 空切片 = 一个都不展开**（"我就要裸节点"）;
	// 要精确控制就列路径 —— 例如评论列表只要 `[]string{"author", "parent"}`,
	// 没必要把每行评论引用的那篇文章正文也拖进响应（真实站点一页 20 条 ≈ 30~80KB）。
	//
	// 注意 nil 与空切片**语义不同**（这是刻意的: 零值必须保持老行为）。
	Expand []string
}

const (
	// DefaultCountLimit 列表页默认统计上限: 精确 count 要扫过全部匹配行。
	// 上限同时决定"能翻到第几页"; 低于它时 total 精确。
	DefaultCountLimit = 10_000
	// CountExact 传 CountNodes 的 countLimit: 精确统计（小表/导出用）。
	CountExact = -1
	// MaxPageSize 单次取行的上限。
	MaxPageSize = 10_000
)

// GetNode 单个读（Fields 完整: 引用 id 已在其中）。不存在返回 ErrNotFound。
//
// 三种入参, 都由**数据**决定而不是猜:
//
//	数字       → 按 id
//	数字串     → 按 id（"12" 与 12 同义 —— URL 路径/模板参数天然是字符串）
//	其它字符串  → 按地址（capabilities.addressable 注入的字段; 见存储层的生成列）
//
// 数字串能放心当 id, 靠的是**地址必须字母开头**这条不变量（types.ValidAddress;
// 它挂在 kind 的 Validate 上 ⇒ 引擎写入也要过 ValidateFields）⇒ 十进制整数与
// 地址空间不相交、不可能歧义。
//
// 踩过一次: 这里原来把**任何字符串**都当地址查, 于是 web.Get / render 传 "2" 去找
// address="2"、回"不存在"（评论插件 target=2 全部 404）。
//
// 地址空间是全表的（唯一索引不带类型谓词）⇒ 也不会有"命中两个类型"这条分支。
func (s *GCM) GetNode(ref any) (*Node, error) {
	if text, ok := ref.(string); ok {
		text = strings.TrimSpace(text)
		id, err := strconv.ParseInt(text, 10, 64)
		if err == nil {
			return s.readNode(nil, id)
		}
		return s.nodeByAddress(text)
	}
	id, err := types.ToID(ref)
	if err != nil {
		return nil, fmt.Errorf("%w: GetNode needs a node id or slug: %v", ErrInvalidQuery, err)
	}
	return s.readNode(nil, id)
}

// nodeByAddress 按地址取节点（**走生成列 address** —— 唯一索引就在它上面;
// 同名的字段路径 `$address` 语义相同但走 json_extract, 用不上索引）。
func (s *GCM) nodeByAddress(address string) (*Node, error) {
	if address == "" {
		return nil, ErrNotFound
	}
	row, err := s.db.Add(`SELECT * FROM nodes WHERE address = #{1}`, address).FetchOne[Node]()
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNotFound
	}
	nodes := []Node{*row}
	err = s.hydrateFields(nil, nodes)
	if err != nil {
		return nil, err
	}
	return &nodes[0], nil
}

// readNode 按 id 取一个节点并补投影。
func (s *GCM) readNode(db *dba.SQL, id int64) (*Node, error) {
	row, err := s.nodeRow(db, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNotFound
	}
	nodes := []Node{*row}
	err = s.hydrateFields(db, nodes)
	if err != nil {
		return nil, err
	}
	return &nodes[0], nil
}

// GetNodesByIDs 按 id 批量读（Fields 完整, 按请求的 id 顺序返回; 缺失的跳过）。
func (s *GCM) GetNodesByIDs(ids []int64) ([]*Node, error) {
	if len(ids) == 0 {
		return []*Node{}, nil
	}
	nodes, err := s.nodesByIDs(nil, ids)
	if err != nil {
		return nil, err
	}
	err = s.hydrateFields(nil, nodes)
	if err != nil {
		return nil, err
	}
	return nodePtrs(nodes), nil
}

// GetNodes 列表读。limit 0 = 不限（调用方自己保证别拉爆）。
func (s *GCM) GetNodes(q NodeQuery, limit, offset int) ([]*Node, error) {
	if limit < 0 || offset < 0 {
		return nil, fmt.Errorf("%w: limit/offset must not be negative", ErrInvalidQuery)
	}
	if limit > MaxPageSize {
		return nil, fmt.Errorf("%w: page size exceeds %d", ErrQueryTooComplex, MaxPageSize)
	}
	query, err := s.nodeListQuery(q)
	if err != nil {
		return nil, err
	}
	if limit > 0 {
		query = query.Add("LIMIT #{1} OFFSET #{2}", limit, offset)
	}
	nodes, err := query.FetchList[Node]()
	if err != nil {
		return nil, err
	}
	// 读投影: 引用 id 补进 Fields（一次批量, SQL 次数与行数无关）。
	err = s.hydrateFields(nil, nodes)
	if err != nil {
		return nil, err
	}
	return nodePtrs(nodes), nil
}

// CountNodes 计数。countLimit 0 = DefaultCountLimit, CountExact = 精确。
// 超上限返回上限值（"至少这么多"）—— 大表列表页不必扫完整个匹配集。
func (s *GCM) CountNodes(q NodeQuery, countLimit int) (int64, error) {
	limit := countLimit
	if limit == 0 {
		limit = DefaultCountLimit
	}
	where, err := s.compileWhere(q)
	if err != nil {
		return 0, err
	}
	var total *int64
	if limit > 0 {
		total, err = s.db.Add(`SELECT COUNT(1) FROM (SELECT nodes.id FROM nodes
			WHERE type = #{1} AND #{2} LIMIT #{3})`, q.Type, where, limit+1).FetchOne[int64]()
	} else {
		total, err = s.db.Add(`SELECT COUNT(1) FROM nodes WHERE type = #{1} AND #{2}`,
			q.Type, where).FetchOne[int64]()
	}
	if err != nil {
		return 0, err
	}
	if total == nil {
		return 0, nil
	}
	if limit > 0 && *total > int64(limit) {
		return int64(limit), nil
	}
	return *total, nil
}

// ── 内部 ────────────────────────────────────────

// nodeListQuery 列表查询（Select + 范围 + 排序）。取行与计数共用同一份过滤。
func (s *GCM) nodeListQuery(q NodeQuery) (*dba.SQL, error) {
	where, err := s.compileWhere(q)
	if err != nil {
		return nil, err
	}
	order, err := s.compiler.Sort(q.Type, q.Sort)
	if err != nil {
		return nil, err
	}
	return s.db.Add(
		`SELECT ${F:*} FROM nodes WHERE type = #{1} AND #{2} ORDER BY #{3}`,
		q.Type, where, order), nil
}

// compileWhere 条件 → SQL 片段。零值 = 不过滤（恒真）。
//
// 两步都收在这里: 前端树 →(结构 + 上限)→ AST →(能力校验 + 编译)→ SQL。
func (s *GCM) compileWhere(q NodeQuery) (dba.Node, error) {
	if q.Type == "" {
		return dba.Node{}, fmt.Errorf("%w: type required", ErrInvalidQuery)
	}
	if q.Where.IsZero() {
		return s.compiler.Where(q.Type, nil)
	}
	expression, err := q.Where.Expr()
	if err != nil {
		return dba.Node{}, err
	}
	return s.compiler.Where(q.Type, expression)
}

// nodesByIDs 按 id 批量取"存储行"（只有标量 fields, 没有引用 id）,
// 按**请求的 id 顺序**返回; 缺失的跳过。
func (s *GCM) nodesByIDs(db *dba.SQL, ids []int64) ([]Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.useDB(db).Add(
		`SELECT * FROM nodes WHERE id IN (#{1|expand})`, ids).FetchList[Node]()
	if err != nil {
		return nil, fmt.Errorf("core: nodes by IDs: %w", err)
	}
	byID := make(map[int64]Node, len(rows))
	for _, node := range rows {
		byID[node.ID] = node
	}
	ordered := make([]Node, 0, len(rows))
	for _, id := range ids {
		if node, ok := byID[id]; ok {
			ordered = append(ordered, node)
		}
	}
	return ordered, nil
}

// nodePtrs 值切片 → 指针切片（读面统一返回 *Node）。
func nodePtrs(nodes []Node) []*Node {
	out := make([]*Node, len(nodes))
	for i := range nodes {
		out[i] = &nodes[i]
	}
	return out
}

// hydrateFields 就地补全引用值: 把这些节点的引用 id 注入各自的 Fields。
//
// **读投影的唯一实现** —— 每个读出口都过这里（内部, 不对外）。
// 一次批量覆盖整批（含无向/对称边两端的可能）: SQL 次数与节点数无关。
func (s *GCM) hydrateFields(db *dba.SQL, nodes []Node) error {
	if len(nodes) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(nodes))
	index := make(map[int64]int, len(nodes))
	for i := range nodes {
		ids = append(ids, nodes[i].ID)
		index[nodes[i].ID] = i
	}
	edges, err := s.useDB(db).Add(`SELECT * FROM edges
		WHERE from_node IN (#{1|expand}) ORDER BY sort, id`, ids).FetchList[Edge]()
	if err != nil {
		return err
	}
	values := make(map[int64]map[string][]int64)
	for _, edge := range edges {
		if _, ok := index[edge.FromNode]; ok {
			appendRefValue(values, edge.FromNode, edge.Field, edge.ToNode)
		}
	}
	for id, i := range index {
		node := &nodes[i]
		td, ok := s.types.Type(node.Type)
		if !ok {
			return fmt.Errorf("core: type %q not defined", node.Type)
		}
		for _, field := range td.Fields {
			kind, ok := s.types.Kind(field.Kind)
			if !ok || (kind.Class() != types.ClassRef && kind.Class() != types.ClassRefList) {
				continue
			}
			refs := values[id][field.Name]
			if kind.Class() == types.ClassRef {
				if len(refs) > 1 {
					return fmt.Errorf("core: single ref %s.%s has %d edges", node.Type, field.Name, len(refs))
				}
				if len(refs) == 1 {
					setRefValue(node, field.Name, refs[0])
				}
				continue
			}
			// 引用 id 用自己的类型, 不用 []any: 读出来的值必须能原样写回。
			items := make([]int64, len(refs))
			copy(items, refs)
			setRefValue(node, field.Name, items)
		}
	}
	return nil
}

// setRefValue 把引用值注入 Fields（Fields 为空时先分配）。
func setRefValue(node *Node, name string, value any) {
	if node.Fields == nil {
		node.Fields = Fields{}
	}
	node.Fields[name] = value
}

func appendRefValue(values map[int64]map[string][]int64, nodeID int64, field string, targetID int64) {
	if values[nodeID] == nil {
		values[nodeID] = make(map[string][]int64)
	}
	values[nodeID][field] = append(values[nodeID][field], targetID)
}
