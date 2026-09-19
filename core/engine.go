package core

import (
	"github.com/kran/dba"
	"github.com/kran/gcmv3/types"
)

// Engine 引擎对外契约（门面 — 消费方依赖此接口, 实现是 GCM）。
//
// 设计原则:
//   - 接口与实现同包（core）— 依赖方向朝下, 循环依赖从结构上不可能
//   - 只含**被消费**的能力（零消费方的方法不该在接口上 —— 加方法请先拿出调用点）
//   - 内部方法（addEdges/splitFields/compileWhere/inEdges 等）不进接口 — 接口 = 对外契约
//
// **写方法的第一个参数是 db *dba.SQL（不是 context.Context）** —— dba 的句柄
// 本身就是「连接 + ctx」的载体（WithCtx 是它的字段, Begin 继承它）:
//
//	nil                        不在调用方事务里 ⇒ 用引擎自持句柄（无 ctx）
//	db.WithCtx(ctx)           不进事务, 但带上请求的取消/超时（HTTP 路径一律这样传）
//	tx（Transaction 回调里的）  加入调用方事务 —— 跨聚合原子性
//
// **读方法不收句柄**: 读没有事务语义, 也不该被卷进调用方未提交的写里 ——
// 它就用引擎自持的句柄。写路径内部统一走 GCM.tx（见 gcm.go）: 句柄已在事务里
// 就直接跑, 不嵌套。
//
// **不在接口上的东西, 以及为什么**:
//
//	内部方法    addEdges / splitFields / compileWhere / inEdges … —— 接口 = 对外契约
//	DB()       逃生舱（拿句柄干引擎不提供的事）—— 不是引擎操作
//	图原语      遍历 / 子树 / 树加载 / OutEdges … —— 暂时不做; 真需要时进**插件**
//	            （可选能力不进内核 —— 但删除限制要用的"谁引用了我"走内部）
//	认证        opaque 凭据 + realm 会话 —— 还没实现（契约在下面的注释里留着）
//	设置        settings —— 不在 core
type Engine interface {
	Types() *types.Types
	Hooks() *HookBus

	// ── 写（hook 挂扩展点 — BeforeCreate/AfterCreate/…, 都在同一个事务里）──

	// CreateNode 建节点: 校验 → BeforeCreate → INSERT → 落边 → AfterCreate。
	// 不修改调用方传入的 Node（内部拷贝）。返回新节点 ID。
	CreateNode(db *dba.SQL, n *Node) (int64, error)
	// PatchNode 差量更新（乐观锁: Revision 必须等于库里的版本）。
	// Fields 用 JSON merge 语义; 引用字段是**替换**（先清旧边再落新边）。
	PatchNode(db *dba.SQL, id int64, patch *NodePatch) error
	// DeleteNode 永久删除。**被引用就不许删**（restrict）—— 错误里带上引用方。
	DeleteNode(db *dba.SQL, id int64) error

	// ── 读（Fields 一律完整: 引用 id 已在其中）──

	// GetNode 单个读。ref 只认数字 id 或**地址**（addressable 类型注入的 address
	// 字段, 全表唯一）; 不存在返回 ErrNotFound。
	GetNode(ref any) (*Node, error)
	// GetNodes 列表读。limit 0 = 不限。
	// **行范围不是参数**: 授权词汇归读层 —— 它把规则与客户端条件 AND 成一个
	// Where 再传进来（客户端的东西只能当子项 ⇒ 只能更窄）。
	GetNodes(q NodeQuery, limit, offset int) ([]*Node, error)
	// GetNodesByIDs 按 id 批量读（按请求顺序, 缺失的跳过）。
	GetNodesByIDs(ids []int64) ([]*Node, error)
	// CountNodes 计数。countLimit 0 = DefaultCountLimit, CountExact = 精确。
	CountNodes(q NodeQuery, countLimit int) (int64, error)

	// ── 展开（读完之后的独立一步: 给已加载的节点补引用目标）──

	// ExpandNodes 就地写 node.Expand。paths 为空 = 不展开;
	// `authors` 出边 / `<-article.categories` 入边 / `categories.parent` 链。
	ExpandNodes(nodes []*Node, paths ...string) ([]*Node, error)
	// ExpandNode 单个 —— ExpandNodes 的一行包装。
	ExpandNode(node *Node, paths ...string) (*Node, error)

	// ── 认证原语（不透明凭据 + realm 绑定的会话）──
	//
	// 内核**不解释**凭据: data 是什么由凭据插件决定（密码比对在 plugin/password）。
	// "这个类型能不能登录" 与角色词表由 types 的 authentication 能力声明。

	// RegisterAuth 原子地建可登录节点 + 绑一条凭据（同一事务 —— 节点建了凭据没
	// 建上, 就是一个永远登不进来的账号）。
	RegisterAuth(db *dba.SQL, nodeType, method, identifier string, data Fields, n *Node) (int64, error)
	// FindAuth 按 (类型, 方式, 标识) 找凭据; 没有返回 (nil, nil)。
	FindAuth(nodeType, method, identifier string) (*AuthMethod, error)
	// AddAuthMethod 给已有节点绑一条凭据（标识被占用 ⇒ 报错）。
	AddAuthMethod(db *dba.SQL, nodeType string, nodeID int64, method, identifier string, data Fields) error
	// SetAuthMethod 设置/替换凭据（改密码 / 换绑）—— 标识属于别的节点时报错。
	SetAuthMethod(db *dba.SQL, nodeType string, nodeID int64, method, identifier string, data Fields) error
	// RemoveAuthMethod 解绑凭据, 但不许删掉最后一个（否则再也登不进来）。
	RemoveAuthMethod(db *dba.SQL, nodeType, method, identifier string) error
	// CreateSession 建会话, 返回**原始**令牌（库里只留 SHA-256）。
	CreateSession(db *dba.SQL, realm string, nodeID int64) (string, error)
	// ValidSession 解令牌: 无效或过期返回 (nil, nil)。纯读（不续期、不清理）。
	ValidSession(token string) (*Session, error)
	// DeleteSession 注销一条会话（登出）。
	DeleteSession(db *dba.SQL, token string) error
	// DeleteNodeSessions 踢掉某个节点的全部会话（改密码 / 封禁之后）。
	DeleteNodeSessions(db *dba.SQL, nodeID int64) error
}

// ── hook 事件名（对称命名 — 写路径扩展点） ──
const (
	HookNodeBeforeCreate = "node.before_create" // 新建前（事务内, 失败回滚 — 审计/默认值）
	HookNodeAfterCreate  = "node.after_create"  // 新建后（事务内 — 索引同步/通知）
	HookNodeBeforeUpdate = "node.before_update" // 更新前（事务内 — 审计/级联检查）
	HookNodeAfterUpdate  = "node.after_update"  // 更新后（事务内 — 索引同步/通知）
	HookNodeBeforeDelete = "node.before_delete" // 永久删除前（事务内 — 级联检查/拒绝删除）
	HookNodeAfterDelete  = "node.after_delete"  // 永久删除后（事务内 — 索引删除/审计）
)

// Engine 编译期断言: GCM 实现完整契约（接口方法增减 → 此处编译报错）。
var _ Engine = (*GCM)(nil)
