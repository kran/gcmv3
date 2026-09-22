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
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kran/cho"
	"github.com/kran/dba"
	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/types"
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite" // sqlite driver 注册
)

// 站点固定路径（basedir 下）。模板/上传等目录等用到时再进来。
const (
	dbFile = "gcm.sqlite"
	// siteFile 站点声明（站点名 + 类型定义）。
	//
	// 名字从 site.yaml 改过来是对的: 这份文件声明的是"**这个站**"（类型只是其中一块,
	// 以后 search/sitemap 的收录范围也可以进来）。老的 site.yaml 仍然认（兜底 +
	// 一条提示日志）, 免得每个站都要同时改。
	siteFile = "site.yaml"
)

// Site 站点 —— 一个引擎 + 一个路由器 + 一处站点目录。
type Site struct {
	basedir string
	db      *dba.SQL
	types   *types.Types
	name    string      // 站点名（site.yaml 的 name）—— 后台左上角显示它
	fields  core.Fields // 站点自己的配置（site.yaml 的 fields: 子树; 框架不解释）
	engine  core.Engine
	render  *Render // 懒创建（Site.Render()）—— 站点没渲染需求就不创建
	router  *cho.Cho[*CmsCtx]
	auth    *AuthRegistry
	// loginLimiter 登录/注册失败限速（配置期可调; 见 limiter.go）。
	loginLimiter *limiter
	// uploadLimit 单文件上传上限（配置期可调; <=0 关闭上传）。
	uploadLimit int64
	// corsOrigins / corsAny API 的跨源来源清单（配置期可调; 见 cors.go）。
	corsOrigins map[string]bool
	corsAny     bool

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

// Option 装配选项（Open 用）。
type Option func(*openOptions)

type openOptions struct {
	logger dba.LogFunc
}

// WithLogger 换掉 SQL 日志。默认只记**错误与慢查询**, 且不写 SQL 文本与参数 ——
// SQL 里可能带登录标识这种敏感值（v2 的站点注释里就专门提过这件事）。
// 排查性能问题时可以传 dba.NewLogger(slog.Default(), 0, true) 把每条 SQL 都打出来。
func WithLogger(fn dba.LogFunc) Option {
	return func(options *openOptions) { options.logger = fn }
}

// Open 开站点: 开库（档位在这里拼死）→ 建引擎的基础表 → 载类型 → 起引擎。
//
// schema 是**基础**的（建表 + 索引, 每条 IF NOT EXISTS ⇒ 幂等）, 直接执行一遍;
// 没有迁移版本管理, 也没有增量更新 —— 那些等真需要时再说。
func Open(basedir string, opts ...Option) (*Site, error) {
	options := openOptions{logger: quietLogger(slog.Default())}
	for _, opt := range opts {
		opt(&options)
	}
	db, err := openDB(filepath.Join(basedir, dbFile), options.logger)
	if err != nil {
		return nil, err
	}
	err = applySchema(db)
	if err != nil {
		_ = db.Pool().Close()
		return nil, err
	}
	decl, err := loadSite(basedir)
	if err != nil {
		_ = db.Pool().Close()
		return nil, err
	}
	engine, err := core.OpenGCM(db, decl.Types)
	if err != nil {
		_ = db.Pool().Close()
		return nil, err
	}
	// web 事件在**配置期之前**定义好 —— 于是 Hook 注册没有"事件还不存在"的时序问题。
	defineWebHooks(engine)
	defineAuthHooks(engine)
	defineAdminHooks(engine)
	site := &Site{
		basedir: basedir, name: decl.Name, fields: decl.Fields,
		db: db, types: decl.Types, engine: engine,
	}
	site.auth = newAuthRegistry(site)
	// 每个 auth 能力类型自动一条同名渠道（站点显式 Register 覆盖它）
	site.auth.registerDefaults()
	site.loginLimiter = newLimiter(defaultLoginFailures, defaultLoginWindow)
	site.uploadLimit = defaultUploadLimit
	site.router = cho.New(site.CmsCtxMaker) // 空 router: 路由留到 Setup
	site.alive.Store(true)
	return site, nil
}

// New 便捷入口: 起不来即 panic（站点启动期 fail loud）。
func New(basedir string, opts ...Option) *Site {
	site, err := Open(basedir, opts...)
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

// Name 站点名（site.yaml 的 name; 空 = 没声明）—— 后台左上角/登录副标题显示它。
func (s *Site) Name() string { return s.name }

// Fields 站点自己的配置（site.yaml 的 fields: 子树）—— 框架不解释里面的键。
//
// 返回**副本**: 调用方改动不会影响站点（"一切皆值" —— 配置是值, 不是共享可变状态）。
func (s *Site) Fields() core.Fields {
	out := make(core.Fields, len(s.fields))
	for key, value := range s.fields {
		out[key] = value
	}
	return out
}

// Field 取站点配置里的一项（取不到返回 nil）。
func (s *Site) Field(key string) any { return s.fields[key] }

// BaseDir 站点根目录（库/类型/static/uploads 都在它下面）—— 站点与插件要读写自己的
// 文件时用它（框架自己不用: 它只知道基于它的固定路径）。
func (s *Site) BaseDir() string { return s.basedir }

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
func openDB(path string, logger dba.LogFunc) (*dba.SQL, error) {
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
	return db.SetLogger(logger), nil
}

// slowQuery 超过它就记一条警告（默认日志只记这个与错误）。
const slowQuery = 500 * time.Millisecond

// quietLogger 默认 SQL 日志: **不写 SQL 文本与参数**（可能带登录标识这类敏感值）,
// 只记错误与慢查询。要全量 SQL 用 web.WithLogger(dba.NewLogger(...))。
func quietLogger(logger *slog.Logger) dba.LogFunc {
	return func(ctx context.Context, begin time.Time, _ string, _ []any, err error) {
		duration := time.Since(begin)
		switch {
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			logger.ErrorContext(ctx, "web: database error", "duration", duration, "err", err)
		case err == nil && duration >= slowQuery:
			logger.WarnContext(ctx, "web: slow query", "duration", duration)
		}
	}
}

// loadSite 读站点声明: name（后台显示用）+ types（交给 types.Load, 校验口径不变）。
//
// 两份文件都开严格解析（KnownFields）: 键名拼错当场报错, 不静默忽略。
// loadSite 读站点声明: name（后台显示用）+ types（交给 types.Load, 校验口径不变）。
//
// 一份声明一个名字（site.yaml）—— 不留旧名兜底: 少一个分支, 拼错路径也不会静默用别的东西。
// 两个解析都开严格模式（KnownFields）⇒ 键名拼错当场报错。
func loadSite(basedir string) (*SiteFile, error) {
	return LoadSiteFile(filepath.Join(basedir, siteFile))
}

// SiteFile 一份站点声明（site.yaml）。
//
// Name 是后台品牌; Types 是类型定义; **Fields 是站点自己的配置**（框架不解释它 ——
// 站点读它, 例如 base_url / oss_bucket / feishu_hooks）。
//
// 为什么放这里而不是仓库级的 sites.yaml: 站点自己的东西就该在站点目录里 ——
// 那份 sites.yaml 只回答"哪个目录 + 认哪些域名"（部署拓扑）, 其余全是站点的事。
// 字段是**自由映射**（框架不校验里面的键 ⇒ 站点怎么用都行, 但拼错键要自己 fail-loud）。
type SiteFile struct {
	Name   string
	Fields core.Fields
	Types  *types.Types
}

// LoadSiteFile 读一份站点声明（name + fields + types）。
//
// 导出是给**站点根目录之外的调用方**用的（例如迁移工具、一次性脚本）—— 校验口径
// 与起站完全一致, 不复制第二份解析。
func LoadSiteFile(path string) (*SiteFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("web: 读站点声明 %s: %w（站点根目录下必须有它）", siteFile, err)
	}
	var doc struct {
		Name   string      `yaml:"name"`
		Fields core.Fields `yaml:"fields"`
		Types  yaml.Node   `yaml:"types"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	err = decoder.Decode(&doc)
	if err != nil {
		return nil, fmt.Errorf("web: 解析 %s: %w", siteFile, err)
	}
	if strings.TrimSpace(doc.Name) == "" {
		// 不拦（但说一声）: 后台左上角会显示为空
		log.Printf("web: %s 缺少 name: —— 后台左上角会显示为空", siteFile)
	}
	// 类型子树按原样交给 types.Load（它自己再开一次严格解析 —— 两边都严, 拼错都报错）
	// yaml.Node 要放在**结构体字段**里才能往返（塞进 map[string]any 会变成 interface{} 炸）
	typesRaw, err := yaml.Marshal(struct {
		Types yaml.Node `yaml:"types"`
	}{Types: doc.Types})
	if err != nil {
		return nil, fmt.Errorf("web: 转 types 子树: %w", err)
	}
	ts := types.New()
	err = ts.Load(typesRaw)
	if err != nil {
		return nil, fmt.Errorf("web: load types: %w", err)
	}
	fields := doc.Fields
	if fields == nil {
		fields = core.Fields{}
	}
	return &SiteFile{Name: strings.TrimSpace(doc.Name), Fields: fields, Types: ts}, nil
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
