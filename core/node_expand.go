// 节点展开（读完之后的独立一步）。
//
// Read 只给**值**（Fields 里是引用 id）; 要引用目标的**节点**才调 Expand。
// 所以展开是显式的、读完之后的第二步 —— 不做"按类型自动展开"（那是第二种形态）。
//
// 一批一段路径**两条查询**: 一条取边、一条取目标节点。SQL 次数与节点数、段数
// 都无关（只与路径段数线性相关）。
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
	// maxExpandEdges 一段关系取边的上限。超了**报错**, 不静默截断
	//（展开缺了一块比失败更难查）。
	maxExpandEdges = 1000
)

// ExpandNodes 给"已经加载好的"节点补上引用目标（就地改 node.Expand, 也返回它）。
//
//	paths 为空 = 不展开
//	authors                  出边引用
//	<-article.categories     入边引用（来源类型必须显式）
//	categories.parent        链（点分隔, 逐段推进）
//	*                        该类型的**所有引用字段**（单层; 键仍是字段名）
//
// 一条路径的**一段** = 3 条查询（取边 / 取目标节点 / 目标节点的读投影）——
// 与节点数无关: 一批节点共用。
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
		for _, path := range paths {
			// 路径按**类型**解析 —— `*` 要按类型展开成它的引用字段
			chains, err := s.expandChains(typeName, path)
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
	}
	return nodes, nil
}

// expandChains 一条路径 → 一条或多条链。
//
//	"*"  该类型剩下的**所有引用字段**（单层）—— 显式要求"全展开", 不是按类型猜。
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

	kind, _ := s.types.Kind(field.Kind)
	single := kind.Class() == types.ClassRef
	key := segment.key()
	for _, node := range nodes {
		if node.Expand == nil {
			node.Expand = map[string]any{}
		}
		values := byNode[node.ID]
		if !single {
			node.Expand[key] = values
			continue
		}
		if len(values) > 1 {
			return fmt.Errorf("core: expand: single ref %s.%s has %d edges",
				typeName, field.Name, len(values))
		}
		if len(values) == 1 {
			node.Expand[key] = values[0]
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
	// 目标的**类型**是来源类型 —— 顺手核一下它声明了什么（不合法就是配置漂移）
	if _, ok := s.types.Type(segment.source); !ok {
		return types.FieldDef{}, "", fmt.Errorf("%w: expand: type %q not defined",
			ErrInvalidQuery, segment.source)
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
	// 批次这一端 → 另一端
	var query string
	if segment.incoming {
		// 来源要按类型筛: 同名的字段可能出现在别的类型上
		query = `SELECT e.* FROM edges e JOIN nodes src ON src.id = e.from_node
			WHERE e.field = #{1} AND src.type = #{2} AND e.to_node IN (#{3|expand})
			ORDER BY e.to_node, e.sort, e.id LIMIT #{4}`
	} else {
		query = `SELECT * FROM edges
			WHERE field = #{1} AND from_node IN (#{3|expand})
			ORDER BY from_node, sort, id LIMIT #{4}`
	}
	args := []any{field.Name, segment.source, ids, maxExpandEdges + 1}
	edges, err := s.db.Add(query, args...).FetchList[Edge]()
	if err != nil {
		return nil, nil, fmt.Errorf("core: expand edges: %w", err)
	}
	if len(edges) > maxExpandEdges {
		return nil, nil, fmt.Errorf("%w: expand %q exceeds %d edges",
			ErrQueryTooComplex, field.Name, maxExpandEdges)
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
