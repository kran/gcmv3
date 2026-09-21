// 认证原语: 不透明凭据 + realm 绑定的会话。
//
// 内核**不理解**凭据: `data` 是什么（密码哈希 / oauth 令牌 / 短信验证码…）由凭据
// 插件决定 —— 密码比对、注册校验那些都在插件里（见 plugin/password）。
// 内核只管三件事:
//
//	一个可登录节点 ↔ 多条凭据（auth_methods, 删节点级联）
//	一个会话 = (realm, node) 的一条登录态（sessions, 只存 token 的 SHA-256）
//	"这个类型能不能登录" 由 types 的 authentication 能力声明（角色词表也在那里）
package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/kran/dba"
)

// ErrLastAuthMethod 不许删掉最后一个凭据 —— 删了这个人再也登不进来。
//
// 是**哨兵错误**（不是随手 errors.New）: 上层要能识别它并回 4xx + 一句人话,
// 而不是当成服务端故障回 500。
var ErrLastAuthMethod = errors.New("core: auth: 这是最后一条登录凭据, 删掉就再也登不进来了")

// AuthMethod 一条凭据（内核只存不解释）。
type AuthMethod struct {
	ID         int64  `db:"id,omitempty" json:"id"`
	NodeType   string `db:"type" json:"node_type"`
	NodeID     int64  `db:"node_id" json:"node_id"`
	Method     string `db:"method" json:"method"`
	Identifier string `db:"identifier" json:"identifier"`
	// Data 不透明凭据（不对外 —— 免得校验逻辑漏到读面）。
	Data      Fields `db:"data" json:"-"`
	CreatedAt int64  `db:"created_at" json:"created_at"`
	UpdatedAt int64  `db:"updated_at" json:"updated_at"`
}

// Session 一条 realm 绑定的会话。TokenHash 永不外发。
type Session struct {
	TokenHash string `db:"token_hash" json:"-"`
	Realm     string `db:"realm" json:"realm"`
	NodeID    int64  `db:"node_id" json:"node_id"`
	ExpiresAt int64  `db:"expires_at" json:"expires_at"`
	CreatedAt int64  `db:"created_at" json:"created_at"`
}

// SessionTTL 会话有效期。
//
// **不过期不续期**（v2 有"过半刷新", 这里去掉了 —— 那让 ValidSession 这个读方法
// 偷偷写库; 需要滑动就由调用方显式重开会话）。
const SessionTTL = 7 * 24 * time.Hour

// RegisterAuth 原子地建"可登录的节点" + 绑一条凭据。
//
// 用 db 参数把两件事放进**同一个事务** —— 节点建了凭据没建上, 就是一个能登录
// 但永远登不进来的账号。
func (s *GCM) RegisterAuth(db *dba.SQL, nodeType, method, identifier string, data Fields, n *Node) (int64, error) {
	err := s.checkAuthType(nodeType)
	if err != nil {
		return 0, err
	}
	if method == "" || identifier == "" {
		return 0, errors.New("core: auth: method and identifier required")
	}
	if n == nil {
		n = &Node{}
	}
	n.Type = nodeType

	var nodeID int64
	err = s.tx(db, func(tx *dba.SQL) error {
		existing, err := s.findAuth(tx, nodeType, method, identifier)
		if err != nil {
			return err
		}
		if existing != nil {
			return fmt.Errorf("%w: auth %s %q", ErrDuplicate, method, identifier)
		}
		id, err := s.CreateNode(tx, n)
		if err != nil {
			return err
		}
		nodeID = id
		now := nowValue()
		m := &AuthMethod{
			NodeType: nodeType, NodeID: id, Method: method, Identifier: identifier,
			Data: data, CreatedAt: now, UpdatedAt: now,
		}
		_, err = tx.Insert("auth_methods", m).Exec()
		err = duplicate(err)
		if err != nil {
			return fmt.Errorf("core: auth: insert method: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return nodeID, nil
}

// AuthMethodsOf 列出某个节点的全部凭据（按方式、标识排序, 输出稳定）。
//
// 不回 data 的内容（AuthMethod.Data 是 json:"-"）—— 调用方要的是"这个人有哪些
// 登录方式、各自标识是什么"; 哈希永远不会离开进程。
func (s *GCM) AuthMethodsOf(nodeType string, nodeID int64) ([]AuthMethod, error) {
	rows, err := s.db.
		Add(`SELECT * FROM auth_methods WHERE type = #{1} AND node_id = #{2} ORDER BY method, identifier`,
			nodeType, nodeID).FetchList[AuthMethod]()
	if err != nil {
		return nil, fmt.Errorf("core: auth methods of %s#%d: %w", nodeType, nodeID, err)
	}
	return rows, nil
}

// FindAuth 按 (类型, 方式, 标识) 找一条凭据; 没有返回 (nil, nil)。
func (s *GCM) FindAuth(nodeType, method, identifier string) (*AuthMethod, error) {
	return s.findAuth(nil, nodeType, method, identifier)
}

func (s *GCM) findAuth(db *dba.SQL, nodeType, method, identifier string) (*AuthMethod, error) {
	return s.useDB(db).Add(`SELECT * FROM auth_methods
		WHERE type = #{1} AND method = #{2} AND identifier = #{3}`,
		nodeType, method, identifier).FetchOne[AuthMethod]()
}

// AddAuthMethod 给已有节点绑一条凭据（标识已被占用 ⇒ 报错）。
func (s *GCM) AddAuthMethod(db *dba.SQL, nodeType string, nodeID int64, method, identifier string, data Fields) error {
	err := s.checkAuthType(nodeType)
	if err != nil {
		return err
	}
	if method == "" || identifier == "" {
		return errors.New("core: auth: method and identifier required")
	}
	return s.tx(db, func(tx *dba.SQL) error {
		err := s.checkAuthNode(tx, nodeType, nodeID)
		if err != nil {
			return err
		}
		existing, err := s.findAuth(tx, nodeType, method, identifier)
		if err != nil {
			return err
		}
		if existing != nil {
			return fmt.Errorf("%w: auth %s %q", ErrDuplicate, method, identifier)
		}
		now := nowValue()
		m := &AuthMethod{
			NodeType: nodeType, NodeID: nodeID, Method: method, Identifier: identifier,
			Data: data, CreatedAt: now, UpdatedAt: now,
		}
		_, err = tx.Insert("auth_methods", m).Exec()
		err = duplicate(err)
		return err
	})
}

// SetAuthMethod 设置某节点上一个**方式**的凭据（“改密码 / 换登录名”）。
//
// 语义: **一个账号的同一个方式只有一条** —— 已经绑过 username 就再绑 username,
// 那是**换掉它**（旧标识一并删掉）, 不是再添一条。
//
// 为什么是这个语义（踩过）: 原来按 (类型, 方式, **标识**) upsert ⇒ “改登录名”会**新插**
// 一条、旧的那条留着 ⇒ 用户以为改名了, 于是旧名字照旧能登录（实测过）, 而凭据面板里
// 显示两条同方式的凭据。
//
// 不同方式仍然可以并存（邮箱 + 手机 + 用户名）—— 那是正常的“多种登录方式”。
// 标识已属于**别的**节点时报 ErrDuplicate（不许抢别人的登录名）。
func (s *GCM) SetAuthMethod(db *dba.SQL, nodeType string, nodeID int64, method, identifier string, data Fields) error {
	err := s.checkAuthType(nodeType)
	if err != nil {
		return err
	}
	if method == "" || identifier == "" {
		return errors.New("core: auth: method and identifier required")
	}
	return s.tx(db, func(tx *dba.SQL) error {
		err := s.checkAuthNode(tx, nodeType, nodeID)
		if err != nil {
			return err
		}
		now := nowValue()
		// ① 同方式、不同标识的行先清掉（含历史遗留的多条）—— 只限**这个账号**。
		//    先删后插在同一个事务里 ⇒ 中间不存在“这个账号没有该方式”的状态。
		_, err = tx.Delete("auth_methods",
			`type = #{1} AND node_id = #{2} AND method = #{3} AND identifier <> #{4}`,
			nodeType, nodeID, method, identifier).Exec()
		if err != nil {
			return err
		}
		// ② 标识已存在: 同一个账号 ⇒ 覆盖 data; 别的账号 ⇒ 报冲突（不许抢）。
		existing, err := s.findAuth(tx, nodeType, method, identifier)
		if err != nil {
			return err
		}
		if existing == nil {
			m := &AuthMethod{
				NodeType: nodeType, NodeID: nodeID, Method: method, Identifier: identifier,
				Data: data, CreatedAt: now, UpdatedAt: now,
			}
			_, err = tx.Insert("auth_methods", m).Exec()
			err = duplicate(err)
			return err
		}
		if existing.NodeID != nodeID {
			// 与“标识已被别的账号用掉”同一类冲突 ⇒ 归一到 ErrDuplicate, 让上层回 409
			return fmt.Errorf("%w: auth %s %q", ErrDuplicate, method, identifier)
		}
		_, err = tx.Update("auth_methods", dba.H{"data": data, "updated_at": now},
			`id = #{1}`, existing.ID).Exec()
		return err
	})
}

// RemoveAuthMethod 解绑一条凭据, 但**不许把最后一个删掉**（否则这个账号再也登不进来）。
func (s *GCM) RemoveAuthMethod(db *dba.SQL, nodeType, method, identifier string) error {
	return s.tx(db, func(tx *dba.SQL) error {
		count, err := tx.Add(`SELECT COUNT(1) FROM auth_methods WHERE type = #{1} AND node_id =
			(SELECT node_id FROM auth_methods WHERE type = #{1} AND method = #{2} AND identifier = #{3})`,
			nodeType, method, identifier).FetchOne[int64]()
		if err != nil {
			return err
		}
		if count == nil || *count <= 1 {
			return ErrLastAuthMethod
		}
		_, err = tx.Delete("auth_methods",
			`type = #{1} AND method = #{2} AND identifier = #{3}`,
			nodeType, method, identifier).Exec()
		return err
	})
}

// CreateSession 建一条 realm 绑定的会话, 返回**原始**令牌（库里只留它的 SHA-256）。
func (s *GCM) CreateSession(db *dba.SQL, realm string, nodeID int64) (string, error) {
	if realm == "" {
		return "", errors.New("core: auth: session realm required")
	}
	node, err := s.nodeRow(db, nodeID)
	if err != nil {
		return "", err
	}
	if node == nil {
		return "", ErrNotFound
	}
	err = s.checkAuthType(node.Type)
	if err != nil {
		return "", err
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	now := nowValue()
	session := &Session{
		TokenHash: sessionTokenHash(token), Realm: realm, NodeID: nodeID,
		ExpiresAt: now + int64(SessionTTL.Seconds()), CreatedAt: now,
	}
	_, err = s.useDB(db).Insert("sessions", session).Exec()
	if err != nil {
		return "", err
	}
	return token, nil
}

// ValidSession 解令牌: 无效或已过期返回 (nil, nil)。
//
// **纯读**（不续期、不清理）—— 过期行留着由 DeleteSession / DeleteNodeSessions
// 或调用方清理; 在这里顺手删是读方法写库, 不值得。
func (s *GCM) ValidSession(token string) (*Session, error) {
	if token == "" {
		return nil, nil
	}
	record, err := s.db.Add(`SELECT * FROM sessions WHERE token_hash = #{1}`,
		sessionTokenHash(token)).FetchOne[Session]()
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, nil
	}
	if record.ExpiresAt <= nowValue() {
		return nil, nil
	}
	return record, nil
}

// DeleteSession 注销一条会话（登出）。
func (s *GCM) DeleteSession(db *dba.SQL, token string) error {
	if token == "" {
		return nil
	}
	_, err := s.useDB(db).Delete("sessions", `token_hash = #{1}`, sessionTokenHash(token)).Exec()
	return err
}

// DeleteNodeSessions 踢掉某个节点的全部会话（改密码 / 封禁之后）。
func (s *GCM) DeleteNodeSessions(db *dba.SQL, nodeID int64) error {
	_, err := s.useDB(db).Delete("sessions", `node_id = #{1}`, nodeID).Exec()
	return err
}

// ── 内部 ────────────────────────────────────────

// checkAuthType 该类型必须声明了 authentication 能力（否则它不能登录）。
func (s *GCM) checkAuthType(nodeType string) error {
	td, ok := s.types.Type(nodeType)
	if !ok {
		return fmt.Errorf("core: auth: type %q not defined", nodeType)
	}
	if td.Capabilities.Authentication == nil {
		return fmt.Errorf("core: auth: type %q is not auth-enabled", nodeType)
	}
	return nil
}

// checkAuthNode 节点存在、类型对得上、能登录。
func (s *GCM) checkAuthNode(db *dba.SQL, nodeType string, nodeID int64) error {
	node, err := s.nodeRow(db, nodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return ErrNotFound
	}
	if node.Type != nodeType {
		return fmt.Errorf("core: auth: node %d is type %q, not %q", nodeID, node.Type, nodeType)
	}
	return nil
}

func sessionTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	buffer := make([]byte, 32)
	_, err := rand.Read(buffer)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
