// Package web 站点层 —— 把内核装成一个能对外服务的站点。
//
// 它拥有内核**不知道**的那一半: **有状态**的那一半 —— 当前请求的身份（Actor）、
// 每个类型的授权策略、字段掩码、通用 API、后台界面、模板渲染。
// 内核只有节点/边/认证原语; "谁能看/改哪些行与字段" 全在这里。
//
// 三阶段装配（解决了插件时序: **中间件必须先于路由**）:
//
//	site, _ := web.Open(basedir)          // ① 初始化: 库 + 类型 + 引擎 + 空 router
//	site.Type("article").OnRead(fn)       // ② 配置期: 策略 / site.Hook(...) / Router()
//	handler := site.Setup()               // ③ 启动: 挂内置路由 → http.Handler
//
// 内置路由只有四类: 静态文件（/static /uploads）、健康探针（/healthz /readyz）、
// 通用 API（/api, E 步）与站点自己经 HookBeforeMount 挂的东西。
package web

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/kran/cho"
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

// Site 站点 —— 一个引擎 + 一个路由器 + 一处站点目录。
type Site struct {
	basedir string
	db      *dba.SQL
	types   *types.Types
	engine  core.Engine
	router  *cho.Cho[*CmsCtx]
	auth    *AuthRegistry

	// secureCookies 认证 cookie 是否只走 HTTPS（生产必须开; 配置期设置）。
	secureCookies bool

	// 每个类型一份授权策略（site.Type("article").OnRead(...) 注册; Setup 前冻结）。
	policies map[string]*TypePolicy

	started   bool      // Setup 已执行（策略此后不可注册）
	setupOnce sync.Once // Setup 幂等（重复调用不会挂两遍路由）
	alive     atomic.Bool
	closeOnce sync.Once
	closeErr  error
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
	// web 事件在**配置期之前**定义好 —— 于是 Hook 注册没有"事件还不存在"的时序问题。
	defineWebHooks(engine)
	defineAuthHooks(engine)
	site := &Site{basedir: basedir, db: db, types: ts, engine: engine}
	site.auth = newAuthRegistry(site)
	site.router = cho.New(site.CmsCtxMaker) // 空 router: 路由留到 Setup
	site.alive.Store(true)
	return site, nil
}

// New 便捷入口: 起不来即 panic（站点启动期 fail loud）。
func New(basedir string) *Site {
	site, err := Open(basedir)
	if err != nil {
		panic(err.Error())
	}
	return site
}

// Close 关站点。幂等; **先摘掉就绪标志**再关连接池 —— 于是 /readyz 在关的过程中
// 就说"closing", 负载均衡不再往这台送新请求。
func (s *Site) Close() error {
	s.closeOnce.Do(func() {
		s.alive.Store(false)
		s.closeErr = s.db.Pool().Close()
	})
	return s.closeErr
}

// Router 底层路由器（站点/插件在配置期挂自己的中间件与路由; 也是 chi 的逃生舱）。
func (s *Site) Router() *cho.Cho[*CmsCtx] { return s.router }

// Auth 渠道注册表（配置期: site.Auth().Register(AuthRealm{…})）。
func (s *Site) Auth() *AuthRegistry { return s.auth }

// SecureCookies 认证 cookie 是否只走 HTTPS。默认 false（本地 http 开发能跑）——
// 上线必须打开, 否则会话令牌会在明文链路上裸奔。
func (s *Site) SecureCookies(on bool) { s.secureCookies = on }

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
