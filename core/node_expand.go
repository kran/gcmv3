// 节点展开（读完之后的独立一步）。
//
// Read 只给**值**（Fields 里是引用 id）; 要引用目标的**节点**才调 Expand。
// 所以展开是显式的、读完之后的第二步 —— 不做"按类型自动展开"（那是第二种形态）。
//
// 一批一段路径固定 3 条查询: 取边 / 取目标节点 / 目标节点的读投影。
// SQL 次数只与路径段数线性相关, 与节点数无关（一批节点共用查询）。
//
// 别名语义: 多个源节点引用同一目标时, 各自 Expand 里挂的是**同一个** *Node。
// 就地修改目标会影响所有引用它的父节点; 需要隔离时调用方自行拷贝。
//
// 占位语义: 展开过的键**一定存在** —— 列表引用恒为 []*Node（可能为空）,
// 单引用可能为 nil。这样调用方能区分"展开了但没有目标"和"根本没展开"。
package core

import (
	"fmt"
	"strings"

	"github.com/kran/gcmv3/types"
)

const (
	// maxExpandPaths 一次展开的路径数上限（客户端可传, 得封顶）。
	maxExpandPaths = 32
	// maxExpandDepth 一条路径的深度上限。
	maxExpandDepth = 4
	// 取边上限随批次大小放大（固定上限会让同一请求"页大 20 能过、页大
	// 100 就炸", 调用方没法预判）: 每个源节点 maxExpandEdgesPerNode 条,
	// 整批封顶 maxExpandEdgesTotal。超了**报错**, 不静默截断
	//（展开缺了一块比失败更难查）。
	maxExpandEdgesPerNode = 200
	maxExpandEdgesTotal   = 5000
)

// ExpandNodes 给"已经加载好的"节点补上引用目标（就地改 node.Expand, 也返回它）。
//
//	paths 为空 = 不展开
//	authors                  出边引用
//	<-article.categories     入边引用（来源类型必须显式）
//	categories.parent        链（点分隔, 逐段推进）
//	*                        该类型的**所有引用字段**（单层; 键仍是字段名）
//
// 路径展开后按首键去重: `["authors", "*"]` 不会把 authors 展开两遍。
func (s *GCM) ExpandNodes(nodes []*Node, paths ...string) ([]*Node, error) {
	if len(paths) == 0 || len(nodes) == 0 {
		return nodes, nil
	}
	if len(paths) > maxExpandPaths {
		return nil, fmt.Errorf("%w: expand exceeds %d paths", ErrQueryTooComplex, maxExpandPaths)
	}
	// 同类型的节点一起展开（一批节点共用查询 —— SQL 次数与节点数无关）
	groups := map[string][]*Node{}
	for _, node := range nodes {
		groups[node.Type] = append(groups[node.Type], node)
	}
	for typeName, group := range groups {
		// 路径按**类型**解析 —— `*` 要按类型展开成它的引用字段
		chains, err := s.resolveChains(typeName, paths)
		if err != nil {
			return nil, err
		}
		for _, chain := range chains {
			err = s.expandChain(group, typeName, chain, 0)
			if err != nil {
				return nil, err
			}
		}
	}
	return nodes, nil
}

// ExpandNode 单个节点的 Expand —— 一行包装。
func (s *GCM) ExpandNode(node *Node, paths ...string) (*Node, error) {
	if node == nil {
		return nil, fmt.Errorf("%w: expand: nil node", ErrInvalidQuery)
	}
	_, err := s.ExpandNodes([]*Node{node}, paths...)
	if err != nil {
		return nil, err
	}
	return node, nil
}

// resolveChains 全部路径 → 去重后的链集合。
//
// 去重按整条链的键（chainKey）: `*` 展开出的字段和显式写的同名路径
// 只保留一条, 避免重复查询、后者覆盖前者。
func (s *GCM) resolveChains(typeName string, paths []string) ([][]expandSegment, error) {
	chains := make([][]expandSegment, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		expanded, err := s.expandChains(typeName, path)
		if err != nil {
			return nil, err
		}
		for _, chain := range expanded {
			id := chainKey(chain)
			if seen[id] {
				continue
			}
			seen[id] = true
			chains = append(chains, chain)
		}
	}
	return chains, nil
}

// expandChains 一条路径 → 一条或多条链。
//
//	"*"  该类型的**所有引用字段**（单层）—— 显式要求"全展开", 不是按类型猜。
//	      挂进去的键仍是字段名本身（不是 "*"）。
func (s *GCM) expandChains(typeName, path string) ([][]expandSegment, error) {
	if strings.TrimSpace(path) != "*" {
		chain, err := parseExpandPath(path)
		if err != nil {
			return nil, err
		}
		return [][]expandSegment{chain}, nil
	}
	td, ok := s.types.Type(typeName)
	if !ok {
		return nil, fmt.Errorf("%w: expand: type %q not defined", ErrInvalidQuery, typeName)
	}
	chains := make([][]expandSegment, 0, len(td.Fields))
	for _, field := range td.Fields {
		if s.types.IsRefKind(field.Kind) {
			chains = append(chains, []expandSegment{{name: field.Name}})
		}
	}
	return chains, nil
}

// expandSegment 展开路径的一段。
type expandSegment struct {
	name     string // 引用字段名
	incoming bool   // <-type.field 形态
	source   string // incoming: 来源类型（显式）
}

// key 挂进 node.Expand 用的键（与输入写法一致）。
func (seg expandSegment) key() string {
	if seg.incoming {
		return "<-" + seg.source + "." + seg.name
	}
	return seg.name
}

// chainKey 整条链的去重键。
func chainKey(chain []expandSegment) string {
	keys := make([]string, len(chain))
	for i, seg := range chain {
		keys[i] = seg.key()
	}
	return strings.Join(keys, ".")
}

// parseExpandPath 路径字符串 → 段序列。点分隔; 入边那一段写成 <-type.field 占两截。
func parseExpandPath(path string) ([]expandSegment, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: expand path is empty", ErrInvalidQuery)
	}
	parts := strings.Split(trimmed, ".")
	segments := make([]expandSegment, 0, len(parts))
	for i := 0; i < len(parts); {
		part := strings.TrimSpace(parts[i])
		if source, ok := strings.CutPrefix(part, "<-"); ok {
			// 入边占两截: <-type.field
			if i+1 >= len(parts) {
				return nil, fmt.Errorf("%w: expand %q: incoming needs <-type.field", ErrInvalidQuery, path)
			}
			field := strings.TrimSpace(parts[i+1])
			if source == "" || field == "" || strings.HasPrefix(field, "<-") {
				return nil, fmt.Errorf("%w: expand %q: incoming needs <-type.field", ErrInvalidQuery, path)
			}
			segments = append(segments, expandSegment{name: field, incoming: true, source: source})
			i += 2
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(part, "->"))
		if name == "" {
			return nil, fmt.Errorf("%w: expand %q: empty field name", ErrInvalidQuery, path)
		}
		segments = append(segments, expandSegment{name: name})
		i++
	}
	if len(segments) > maxExpandDepth {
		return nil, fmt.Errorf("%w: expand %q exceeds depth %d", ErrQueryTooComplex, path, maxExpandDepth)
	}
	return segments, nil
}

// expandChain 推进一段, 然后拿目标节点递归下一段。
func (s *GCM) expandChain(nodes []*Node, typeName string, chain []expandSegment, at int) error {
	if len(nodes) == 0 {
		// 上一段没展开出任何目标: 后面的段自然为空, 不再发查询
		//（也避免空 id 集进 IN (...)）。
		return nil
	}
	segment := chain[at]
	field, targetType, err := s.expandField(typeName, segment)
	if err != nil {
		return err
	}
	ids := make([]int64, len(nodes))
	for i, node := range nodes {
		ids[i] = node.ID
	}
	targets, byNode, err := s.expandTargets(ids, field, segment, targetType)
	if err != nil {
		return err
	}

	// 单/多的基数只对**出边**成立: 入边时字段基数说的是来源那一侧,
	// 多个来源可以指向同一个目标, 所以入边恒为列表。
	kind, _ := s.types.Kind(field.Kind)
	single := !segment.incoming && kind.Class() == types.ClassRef
	key := segment.key()
	for _, node := range nodes {
		if node.Expand == nil {
			node.Expand = map[string]any{}
		}
		values := byNode[node.ID]
		if !single {
			if values == nil {
				values = []*Node{}
			}
			node.Expand[key] = values
			continue
		}
		switch len(values) {
		case 0:
			node.Expand[key] = nil
		case 1:
			node.Expand[key] = values[0]
		default:
			return fmt.Errorf("core: expand: single ref %s.%s on node %d has %d edges",
				typeName, field.Name, node.ID, len(values))
		}
	}

	if at+1 >= len(chain) {
		return nil
	}
	return s.expandChain(targets, targetType, chain, at+1)
}

// expandField 解析一段关系: 它在 typeName 上是引用字段, 返回字段与目标类型。
func (s *GCM) expandField(typeName string, segment expandSegment) (types.FieldDef, string, error) {
	owner := typeName
	if segment.incoming {
		owner = segment.source
	}
	// 入边时 Field 查的就是来源类型: 查得到字段 ⇒ 类型存在, 不必再单独核。
	field, ok := s.types.Field(owner, segment.name)
	if !ok {
		return types.FieldDef{}, "", fmt.Errorf("%w: expand: %s.%s does not exist",
			ErrInvalidField, owner, segment.name)
	}
	if !s.types.IsRefKind(field.Kind) {
		return types.FieldDef{}, "", fmt.Errorf("%w: expand: %s.%s is not a ref",
			ErrInvalidField, owner, segment.name)
	}
	if !segment.incoming {
		return field, field.To, nil
	}
	// 入边: 来源字段必须指向我们（否则这段路径本身没意义）
	if field.To != typeName {
		return types.FieldDef{}, "", fmt.Errorf("%w: expand: %s.%s does not target %s",
			ErrInvalidField, segment.source, segment.name, typeName)
	}
	return field, segment.source, nil
}

// expandTargets 取这一批节点在这一段关系上的目标。
//
// 返回 (目标节点集, 每个源节点 → 目标节点列表)。边序保留（sort, id）。
func (s *GCM) expandTargets(
	ids []int64,
	field types.FieldDef,
	segment expandSegment,
	targetType string,
) ([]*Node, map[int64][]*Node, error) {
	limit := min(len(ids)*maxExpandEdgesPerNode, maxExpandEdgesTotal)

	// 批次这一端 → 另一端。两个方向分开建查询, 各带各的参数。
	var edges []Edge
	var err error
	if segment.incoming {
		// 来源要按类型筛: 同名的字段可能出现在别的类型上
		edges, err = s.db.Add(`SELECT e.* FROM edges e JOIN nodes src ON src.id = e.from_node
			WHERE e.field = #{1} AND src.type = #{2} AND e.to_node IN (#{3|expand})
			ORDER BY e.to_node, e.sort, e.id LIMIT #{4}`,
			field.Name, segment.source, ids, limit+1).FetchList[Edge]()
	} else {
		edges, err = s.db.Add(`SELECT * FROM edges
			WHERE field = #{1} AND from_node IN (#{2|expand})
			ORDER BY from_node, sort, id LIMIT #{3}`,
			field.Name, ids, limit+1).FetchList[Edge]()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("core: expand edges: %w", err)
	}
	if len(edges) > limit {
		return nil, nil, fmt.Errorf("%w: expand %q exceeds %d edges for %d nodes",
			ErrQueryTooComplex, field.Name, limit, len(ids))
	}

	targetIDs := make([]int64, 0, len(edges))
	seen := make(map[int64]bool, len(edges))
	for _, edge := range edges {
		targetID := edge.ToNode
		if segment.incoming {
			targetID = edge.FromNode
		}
		if !seen[targetID] {
			seen[targetID] = true
			targetIDs = append(targetIDs, targetID)
		}
	}
	targets, err := s.nodesByIDs(nil, targetIDs)
	if err != nil {
		return nil, nil, err
	}
	// 读投影: 展开出来的目标也是"读出来的节点" ⇒ Fields 必须完整
	//（否则目标上的引用字段会是空的, 再写回去就丢了）
	err = s.hydrateFields(nil, targets)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[int64]*Node, len(targets))
	for i := range targets {
		if targets[i].Type != targetType {
			return nil, nil, fmt.Errorf("core: expand: target %d is type %q, expected %q",
				targets[i].ID, targets[i].Type, targetType)
		}
		byID[targets[i].ID] = &targets[i]
	}

	byNode := make(map[int64][]*Node, len(ids))
	for _, edge := range edges {
		owner, targetID := edge.FromNode, edge.ToNode
		if segment.incoming {
			owner, targetID = edge.ToNode, edge.FromNode
		}
		if target := byID[targetID]; target != nil {
			byNode[owner] = append(byNode[owner], target)
		}
	}
	return nodePtrs(targets), byNode, nil
}
