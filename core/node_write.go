// 节点写路径（dba 手写 — Node 是值模型, 不走 Dao 泛型）。
//
// 三个写操作:
//
//	CreateNode  全量插入（校验 → before hook → 写 nodes → 落边 → after hook）
//	PatchNode   差量更新（乐观锁 + fields json_patch + 引用**替换**）
//	DeleteNode  永久删除（**被引用就不许删** —— restrict）
//
// 三者各自一个事务。读（nodeRow）也在本文件: 写路径要读当前行做校验/钩子。
package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kran/dba"
	"github.com/kran/gcmv3/types"
)

var (
	// ErrNotFound 目标节点不存在。
	ErrNotFound = errors.New("core: node not found")
	// ErrRevisionConflict 节点在客户端读取之后已被别的写入改过。
	ErrRevisionConflict = errors.New("core: node revision conflict")
	// ErrInvalidFields 提交的字段没过 schema 校验（缺必填/类型不对/不可变/重复引用）。
	ErrInvalidFields = errors.New("core: invalid fields")
)

// invalidFields 包装 schema/值校验错误（Web 边界据此返回 422）。
func invalidFields(err error) error {
	return fmt.Errorf("%w: %w", ErrInvalidFields, err)
}

// nodeRow 按 id 取"存储行"（只有标量 fields, 没有引用 id）。
// 写入路径与索引重建用它; 对外读走读投影（引用 id 注入 fields）。
// 不存在返回 (nil, nil)。
func (s *GCM) nodeRow(db *dba.SQL, id int64) (*Node, error) {
	node, err := s.useDB(db).Select("nodes", `id = #{1}`, id).FetchOne[Node]()
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, nil
	}
	return node, nil
}

// CreateNode 建节点: 校验 → 事务（BeforeCreate → INSERT → ref 落边 → AfterCreate）。
// 返回新节点 ID; **不修改调用方传入的 Node**（内部拷贝）。
// 未知字段直接报错 —— 拼写错误与 schema 漂移不该被静默吞掉。
func (s *GCM) CreateNode(db *dba.SQL, n *Node) (int64, error) {
	if n == nil {
		return 0, errors.New("core: create: nil node")
	}
	td, ok := s.types.Type(n.Type)
	if !ok {
		return 0, fmt.Errorf("core: type %q not defined", n.Type)
	}
	fields, err := s.types.ApplyDefaults(n.Type, n.Fields)
	if err != nil {
		return 0, invalidFields(err)
	}
	err = s.types.ValidateFields(n.Type, fields)
	if err != nil {
		return 0, invalidFields(err)
	}
	// 内部拷贝, 不触碰调用方。BeforeCreate 可以补字段, 所以事务内会**再次校验**,
	// 然后才拆分引用。
	node := *n
	node.ID = 0
	node.Fields = Fields(fields)
	node.Revision = 1
	node.CreatedAt = nowValue()
	node.UpdatedAt = node.CreatedAt

	var id int64
	err = s.tx(db, func(tx *dba.SQL) error {
		err := s.hooks.Fire(HookNodeBeforeCreate, tx, &node)
		if err != nil {
			return err
		}
		if node.Type != td.Name {
			return errors.New("core: create: type is immutable")
		}
		node.ID = 0
		node.Revision = 1
		err = s.types.ValidateFields(node.Type, node.Fields)
		if err != nil {
			return invalidFields(err)
		}
		scalar, refs, err := splitFields(td, s.types, node.Fields)
		if err != nil {
			return err
		}
		node.Fields = scalar
		result, err := tx.Insert("nodes", &node).Exec()
		if err != nil {
			return err
		}
		id, err = result.LastInsertId()
		if err != nil {
			return err
		}
		node.ID = id
		err = addEdges(tx, s.types, td, id, refs)
		if err != nil {
			return err
		}
		return s.hooks.Fire(HookNodeAfterCreate, tx, &node)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// PatchNode 差量更新: 非 nil 列（dba.Update map）+ fields 用 json_patch 合并。
// 空 patch（全 nil + fields 空）⇒ 零 UPDATE（幂等）。
//
// 乐观锁: patch.Revision 必须等于库里的版本, 成功后版本 +1。
func (s *GCM) PatchNode(db *dba.SQL, id int64, patch *NodePatch) error {
	if patch == nil {
		return errors.New("core: patch: nil patch")
	}
	existing, err := s.nodeRow(db, id)
	if err != nil {
		return err
	}
	if existing == nil {
		return ErrNotFound
	}
	if len(patch.Fields) == 0 {
		return nil
	}
	if patch.Revision == nil || *patch.Revision <= 0 {
		return fmt.Errorf("%w: revision required", ErrInvalidFields)
	}
	td, ok := s.types.Type(existing.Type)
	if !ok {
		return fmt.Errorf("core: type %q not defined", existing.Type)
	}

	return s.tx(db, func(tx *dba.SQL) error {
		patch.Type = existing.Type // 钩子要按类型判断（内部字段, 客户端传不进来）
		err := s.hooks.Fire(HookNodeBeforeUpdate, tx, patch)
		if err != nil {
			return err
		}
		err = s.types.ValidatePatchFields(existing.Type, patch.Fields)
		if err != nil {
			return invalidFields(err)
		}

		// 同一套拆分: 标量进 fields, 引用值 → 目标 id 列表（准备替换边）
		scalarPatch, refPatch, err := splitFields(td, s.types, patch.Fields)
		if err != nil {
			return err
		}

		cols := map[string]any{}
		if len(scalarPatch) > 0 {
			body, err := json.Marshal(scalarPatch)
			if err != nil {
				return fmt.Errorf("core: patch fields: %w", err)
			}
			cols["fields"] = dba.Expr(`json_patch(fields, #{1})`, string(body))
		}
		if len(cols) == 0 && len(refPatch) == 0 {
			return nil
		}

		cols["updated_at"] = nowValue()
		cols["revision"] = dba.Expr(`revision + 1`)
		result, err := tx.Update("nodes", cols, `id = #{1} AND revision = #{2}`, id, *patch.Revision).Exec()
		if err != nil {
			return err
		}
		updatedRows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if updatedRows == 0 {
			return ErrRevisionConflict
		}

		// 引用字段: 先清旧边再落新边（"替换"语义, 不是叠边）。
		for name := range refPatch {
			field, _ := types.FieldByName(td, name)
			err = deleteFieldEdges(tx, id, field)
			if err != nil {
				return err
			}
		}
		if len(refPatch) > 0 {
			err = addEdges(tx, s.types, td, id, refPatch)
			if err != nil {
				return err
			}
		}

		updated, err := tx.Select("nodes", `id = #{1}`, id).FetchOne[Node]()
		if err != nil {
			return err
		}
		if updated == nil {
			return fmt.Errorf("core: update: node %d vanished in tx", id)
		}
		return s.hooks.Fire(HookNodeAfterUpdate, tx, updated)
	})
}

// ── 删除 ─────────────────────────────────────

// DeleteRestrictedError 节点被引用而不能删除（内核唯一的删除语义）。
type DeleteRestrictedError struct {
	NodeID     int64  `json:"node_id"`
	References []Edge `json:"references"`
}

func (e *DeleteRestrictedError) Error() string {
	parts := make([]string, 0, len(e.References))
	for _, ref := range e.References {
		parts = append(parts, fmt.Sprintf("node %d.%s", ref.FromNode, ref.Field))
	}
	return fmt.Sprintf("%v: node %d is referenced by %s",
		ErrDeleteRestricted, e.NodeID, strings.Join(parts, ", "))
}

func (e *DeleteRestrictedError) Unwrap() error { return ErrDeleteRestricted }

// DeleteNode 永久删除一个节点。被引用就不许删 —— 整个序列一个事务。
func (s *GCM) DeleteNode(db *dba.SQL, id int64) error {
	return s.tx(db, func(tx *dba.SQL) error {
		node, err := tx.Select("nodes", `id = #{1}`, id).FetchOne[Node]()
		if err != nil {
			return err
		}
		if node == nil {
			return ErrNotFound
		}
		err = s.hooks.Fire(HookNodeBeforeDelete, tx, id)
		if err != nil {
			return err
		}
		references, err := s.inEdges(tx, id)
		if err != nil {
			return err
		}
		if len(references) > 0 {
			return &DeleteRestrictedError{NodeID: id, References: references}
		}
		_, err = tx.Delete("edges", `from_node = #{1} OR to_node = #{1}`, id).Exec()
		if err != nil {
			return err
		}
		result, err := tx.Delete("nodes", `id = #{1}`, id).Exec()
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrNotFound
		}
		return s.hooks.Fire(HookNodeAfterDelete, tx, id)
	})
}
