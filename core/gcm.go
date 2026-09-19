package core

import (
	"fmt"

	"github.com/kran/dba"
	"github.com/kran/gcmv3/types"
	_ "modernc.org/sqlite" // sqlite driver 注册
)

// GCM 核心引擎 — 每站点一个实例, 绑定本站 db + 本站类型系统。
// 节点 CRUD 与引用落边是一个事务（ref 字段值进 edges, fields 只存标量）。
type GCM struct {
	db       *dba.SQL
	types    *types.Types
	hooks    *HookBus
	compiler *Compiler // 条件与排序的编译器（绑定 types, 无状态可共用）
}

// OpenGCM 建引擎: 定义标准 hook 事件。
// 失败返回错误 — 进程入口自己决定是否致命（测试/嵌入场景不 panic）。
//
// **引擎不做启动期 DDL**: 表与索引全部来自 `core/migrations/*.sql`（schema 的
// 事实来源; 迁移执行器还没做, 暂时由调用方建表 —— 见 node_write_test.go 的
// applySchema）。连接档位（WAL / foreign_keys / busy_timeout）也由开库的那一层
// 在 DSN 里定死 —— 在这里查一遍等于把"配置错误"推迟到运行期才响。
func OpenGCM(db *dba.SQL, ts *types.Types) (*GCM, error) {
	s := &GCM{
		db:       db,
		types:    ts,
		hooks:    NewHookBus(),
		compiler: NewCompiler(ts),
	}

	//define hooks
	err := s.hooks.Define(map[string]any{
		HookNodeBeforeCreate: func(*dba.SQL, *Node) error { return nil },
		HookNodeAfterCreate:  func(*dba.SQL, *Node) error { return nil },
		HookNodeBeforeUpdate: func(*dba.SQL, *NodePatch) error { return nil },
		HookNodeAfterUpdate:  func(*dba.SQL, *Node) error { return nil },
		HookNodeBeforeDelete: func(*dba.SQL, int64) error { return nil },
		HookNodeAfterDelete:  func(*dba.SQL, int64) error { return nil },
	})
	if err != nil {
		return nil, fmt.Errorf("core: define standard hooks: %w", err)
	}

	return s, nil
}

// Hooks 站点级 hook 总线。
func (s *GCM) Hooks() *HookBus { return s.hooks }

// Types 类型系统。
func (s *GCM) Types() *types.Types { return s.types }

// DB 底层数据库句柄（逃生舱）。
func (s *GCM) DB() *dba.SQL { return s.db }

// ── 句柄归一 ────────────────────────────────
//
// 写方法收 `db *dba.SQL` 而不是 context.Context: dba 的句柄本身就是
// 「连接 + ctx」的载体（WithCtx 是它的字段, Begin 继承）。
//
//	nil                        不在调用方事务里 ⇒ 用引擎自持句柄
//	db.WithCtx(ctx)           不进事务, 但带上请求的取消/超时
//	tx（Transaction 回调里的）  加入调用方事务 —— 跨聚合原子性

// useDB 归一数据库句柄：nil 用引擎自持的。
func (s *GCM) useDB(db *dba.SQL) *dba.SQL {
	if db == nil {
		return s.db
	}
	return db
}

// tx 在 db 上执行 fn。写路径统一走它: db 已经在事务里 ⇒ dba 直接跑 fn（不嵌套）;
// 否则开一个新事务。
func (s *GCM) tx(db *dba.SQL, fn func(*dba.SQL) error) error {
	return s.useDB(db).Transaction(fn)
}
