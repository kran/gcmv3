package core

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kran/dba"
	"github.com/kran/gcmv3/types"
)

// Edge 引用（edges 表的行 — 类型系统不可见, 用户只见"引用字段"）。
type Edge struct {
	ID        int64  `db:"id,omitempty" json:"id"`
	FromNode  int64  `db:"from_node" json:"from_node"`
	Field     string `db:"field" json:"field"`
	ToNode    int64  `db:"to_node" json:"to_node"`
	Sort      int    `db:"sort" json:"sort"`
	CreatedAt int64  `db:"created_at" json:"created_at"`
}

// inEdges 谁在引用这个节点 —— 删除的唯一限制条件（restrict）。
//
// 内部（收句柄: 删除检查要在同一个事务里看）。引用列表由 DeleteRestrictedError
// 带给调用方 —— "界面上列出的引用"和"挡住删除的原因"走同一条路径, 天然一致,
// 所以不需要另开一个对外的读方法。
//
// **单表查询**: 只扫 edges 的 to_node。
func (s *GCM) inEdges(db *dba.SQL, nodeID int64) ([]Edge, error) {
	edges, err := s.useDB(db).Add(
		`SELECT * FROM edges WHERE to_node = #{1} ORDER BY id`, nodeID).FetchList[Edge]()
	if err != nil {
		return nil, fmt.Errorf("core: in edges: %w", err)
	}
	return edges, nil
}

// ── 引用落边（引擎内部 — Create/Patch 用） ────────

// splitFields 把提交的 fields 拆成两半: 标量（落节点 fields JSON）与
// **引用目标 id**（落 edges）—— "是不是引用"由 kind 自己说了算。
//
// 一趟出结果: 分类与"值 → id"是同一件事的两半, 中间那个 map[string]any
// 只是过路（曾经分成 splitRefs + refIDs 两层）。重复目标在这里就挡掉。
func splitFields(td types.TypeDef, ts *types.Types, fields map[string]any) (map[string]any, map[string][]int64, error) {
	scalar := map[string]any{}
	refs := map[string][]int64{}
	for name, value := range fields {
		field, ok := types.FieldByName(td, name)
		if !ok {
			return nil, nil, fmt.Errorf("core: field %q not on type %q", name, td.Name)
		}
		if !ts.IsRefKind(field.Kind) {
			scalar[name] = value
			continue
		}
		if value == nil {
			continue // 没给 = 不落边
		}
		ids, err := refTargetIDs(ts, field, value)
		if err != nil {
			return nil, nil, err
		}
		seen := make(map[int64]bool, len(ids))
		for _, id := range ids {
			if seen[id] {
				return nil, nil, invalidFields(fmt.Errorf("%q.%s contains duplicate target %d", td.Name, name, id))
			}
			seen[id] = true
		}
		refs[name] = ids
	}
	return scalar, refs, nil
}

// addEdges 落边（引用目标 id 已经由 splitFields 校验过）。
func addEdges(tx *dba.SQL, ts *types.Types, td types.TypeDef, from int64, refs map[string][]int64) error {
	for fieldName, ids := range refs {
		field, ok := types.FieldByName(td, fieldName)
		if !ok {
			return fmt.Errorf("core: field %q not on type %q", fieldName, td.Name)
		}
		for position, targetID := range ids {
			_, err := insertEdge(tx, ts, td, field, from, targetID, position)
			if err != nil {
				return fmt.Errorf("core: %q.%s -> %d: %w", td.Name, fieldName, targetID, err)
			}
		}
	}
	return nil
}

// checkTarget 拒绝不存在与类型不对的目标。
//
// 标量读必须写**显式 SQL**: dba.Select 的 `${F:*}` 会展开成整表列（给实体读用的）,
// 标量 dest 会撞上 "expected N destination arguments in Scan"。
func checkTarget(tx *dba.SQL, id int64, wantType string) error {
	target, err := tx.Add(`SELECT type FROM nodes WHERE id = #{1}`, id).FetchOne[string]()
	if err != nil {
		return err
	}
	if target == nil {
		return fmt.Errorf("%w: target %d", ErrNotFound, id)
	}
	if *target != wantType {
		return fmt.Errorf("target %d is type %q, want %q", id, *target, wantType)
	}
	return nil
}

func insertEdge(tx *dba.SQL, ts *types.Types, td types.TypeDef, field types.FieldDef, from, to int64, sort int) (int64, error) {
	err := checkTarget(tx, to, field.To)
	if err != nil {
		return 0, err
	}
	// 自引用不许: 传递关系（层级 parent）自环即死循环; 其它引用自环无意义。
	if from == to {
		return 0, errors.New("self reference is not allowed")
	}
	if field.Transitive {
		cycle, err := wouldCreateCycle(tx, td.Name, field.Name, from, to)
		if err != nil {
			return 0, err
		}
		if cycle {
			return 0, errors.New("reference would create a cycle")
		}
	}
	result, err := tx.Insert("edges", map[string]any{
		"from_node":  from,
		"field":      field.Name,
		"to_node":    to,
		"sort":       sort,
		"created_at": nowValue(),
	}).Exec()
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: edges") {
			return 0, fmt.Errorf("%w: %s.%s", ErrRelationCardinality, td.Name, field.Name)
		}
		return 0, err
	}
	return result.LastInsertId()
}

func wouldCreateCycle(tx *dba.SQL, typeName, field string, from, to int64) (bool, error) {
	found, err := tx.Add(`WITH RECURSIVE reach(id) AS (
		SELECT #{1}
		UNION
		SELECT e.to_node FROM edges e JOIN reach r ON e.from_node = r.id
		JOIN nodes n ON n.id = e.from_node
		WHERE e.field = #{2} AND n.type = #{3}
	)
	SELECT 1 FROM reach WHERE id = #{4} LIMIT 1`, to, field, typeName, from).FetchOne[int]()
	if err != nil {
		return false, err
	}
	return found != nil, nil
}

// refTargetIDs 引用字段的值 → 目标 id 列表（ref 单个包一层, refs 原样）。
func refTargetIDs(ts *types.Types, field types.FieldDef, value any) ([]int64, error) {
	kind, ok := ts.Kind(field.Kind)
	if !ok {
		return nil, fmt.Errorf("core: unknown kind %q", field.Kind)
	}
	switch kind.Class() {
	case types.ClassRef:
		id, err := types.ToID(value)
		if err != nil {
			return nil, fmt.Errorf("core: ref value: %w", err)
		}
		return []int64{id}, nil
	case types.ClassRefList:
		// 读投影给的是 []int64（读出来的值要能原样写回）; JSON 解码给的是 []any。
		var items []any
		switch list := value.(type) {
		case []int64:
			items = make([]any, len(list))
			for i := range list {
				items[i] = list[i]
			}
		case []any:
			items = list
		default:
			return nil, fmt.Errorf("core: refs value: expects array, got %T", value)
		}
		ids := make([]int64, 0, len(items))
		for i, item := range items {
			id, err := types.ToID(item)
			if err != nil {
				return nil, fmt.Errorf("core: refs value[%d]: %w", i, err)
			}
			ids = append(ids, id)
		}
		return ids, nil
	default:
		return nil, fmt.Errorf("core: %s is not a ref kind", field.Kind)
	}
}

var (
	// ErrRelationCardinality 会破坏 ref/refs 的数据库不变量。
	ErrRelationCardinality = errors.New("core: relation cardinality violation")
	// ErrDeleteRestricted 有入引用, 不允许永久删除。
	ErrDeleteRestricted = errors.New("core: delete restricted")
)

// deleteFieldEdges 清掉某节点在某引用字段上的出边（引用是**有向**的:
// 别人指向它的边不归它管 —— 那是别人的字段）。
func deleteFieldEdges(tx *dba.SQL, nodeID int64, field types.FieldDef) error {
	_, err := tx.Delete("edges", `from_node = #{1} AND field = #{2}`, nodeID, field.Name).Exec()
	return err
}

// nowValue 节点/边时间列的当前值。
//
// TODO(待定): Node.CreatedAt 是 int64, 而 types 的时间表示是**格式字符串**
// （见 types/timestamp.go 的理由: 定宽 ⇒ 字典序 = 时间序, 前端不必猜单位）。
// 两种表示不该并存 —— 这一处是唯一的写入点, 定了改这里 + 迁移的列类型。
func nowValue() int64 { return time.Now().Unix() }
