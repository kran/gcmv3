// Package web 站点层 —— 把内核装成一个能对外服务的站点。
//
// 它拥有内核**不知道**的那一半: **有状态**的那一半 —— 当前请求的身份（Actor）、
// 每个类型的授权策略、字段掩码、通用 API、后台界面、模板渲染。
// 内核只有节点/边/认证原语; "谁能看/改哪些行与字段" 全在这里。
//
// 装配顺序（先建、再配置、最后启动 —— 中间件必须先于路由挂上）:
//
//	site, _ := web.Open(basedir)   // ① 开库 + 类型 + 引擎（校验就绪）
//	site.Hook(web.HookRender, fn)  // ② 配置期: 注册策略/钩子/模板函数
//	handler := site.Start()        // ③ 启动: 挂路由, 交出 http.Handler
package web

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/kran/dba"
	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/types"
	_ "modernc.org/sqlite" // sqlite driver 注册
)

// 站点固定路径（basedir 下）。模板/上传等目录等用到时再进来。
const (
	dbFile    = "gcm.sqlite"
	typesFile = "types.yaml"
)

// Site 站点 —— 一个引擎 + 一处站点目录。
type Site struct {
	basedir string
	db      *dba.SQL
	types   *types.Types
	engine  core.Engine

	// 每个类型一份授权策略（site.Type("article").OnRead(...) 注册）。
	policies map[string]*TypePolicy
}

// Open 开站点: 开库（档位在这里拼死）→ 建引擎的基础表 → 载类型 → 起引擎。
//
// schema 是**基础**的（建表 + 索引, 每条 IF NOT EXISTS ⇒ 幂等）, 直接执行一遍;
// 没有迁移版本管理, 也没有增量更新 —— 那些等真需要时再说。
func Open(basedir string) (*Site, error) {
	db, err := openDB(filepath.Join(basedir, dbFile))
	if err != nil {
		return nil, err
	}
	err = applySchema(db)
	if err != nil {
		_ = db.Pool().Close()
		return nil, err
	}
	ts, err := loadTypes(filepath.Join(basedir, typesFile))
	if err != nil {
		_ = db.Pool().Close()
		return nil, err
	}
	engine, err := core.OpenGCM(db, ts)
	if err != nil {
		_ = db.Pool().Close()
		return nil, err
	}
	return &Site{basedir: basedir, db: db, types: ts, engine: engine}, nil
}

// New 便捷入口: 起不来即 panic（站点启动期 fail loud）。
func New(basedir string) *Site {
	site, err := Open(basedir)
	if err != nil {
		panic(err.Error())
	}
	return site
}

// Close 关站点（关连接池）。
func (s *Site) Close() error { return s.db.Pool().Close() }

// DB 底层句柄（逃生舱）。
func (s *Site) DB() *dba.SQL { return s.db }

// Engine 引擎。
func (s *Site) Engine() core.Engine { return s.engine }

// Types 类型系统。
func (s *Site) Types() *types.Types { return s.types }

// handle 引擎方法的第一个参数: 绑定请求 ctx ⇒ 客户端断开即取消查询。
// （nil = 引擎自持句柄, 脱离请求生命周期。）
func (s *Site) handle(ctx context.Context) *dba.SQL { return s.db.WithCtx(ctx) }

// openDB 开库并把 SQLite 档位**拼死**。
//
// 连接是这一层开的, 所以档位不可能被站点拼错 —— 这正是内核不再做运行时档位校验
// 的原因。三档各有明确后果:
//
//	journal_mode  WAL            非 WAL 下读者与写者互撞排他锁 ⇒ 并发下随机失败
//	foreign_keys  1              级联删除/引用完整性都靠它（删节点清凭据与会话）
//	busy_timeout  5000           撞锁等待而不是立即 SQLITE_BUSY（dba 不做重试）
func openDB(path string) (*dba.SQL, error) {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		err := os.MkdirAll(dir, 0o755)
		if err != nil {
			return nil, fmt.Errorf("web: mkdir db dir: %w", err)
		}
	}
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := dba.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("web: open db: %w", err)
	}
	return db.SetLogger(dba.NewLogger(slog.Default(), 0, false)), nil
}

func loadTypes(path string) (*types.Types, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("web: read types: %w", err)
	}
	ts := types.New()
	err = ts.Load(data)
	if err != nil {
		return nil, fmt.Errorf("web: load types: %w", err)
	}
	return ts, nil
}

// applySchema 建引擎的基础表（幂等: DDL 全是 IF NOT EXISTS）。
//
// 一个事务里跑完: 多条语句一次 Exec, 中途失败不会留下半个 schema。
// 站点自己的表由站点自己建 —— 引擎只带自己那几张。
func applySchema(db *dba.SQL) error {
	err := db.Transaction(func(tx *dba.SQL) error {
		_, err := tx.Add(core.Schema()).Exec()
		return err
	})
	if err != nil {
		return fmt.Errorf("web: apply schema: %w", err)
	}
	return nil
}
