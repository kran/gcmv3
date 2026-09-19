-- +goose Up

-- 凭据: 一条 = 一个登录标识（email / phone / wechat / oauth sub …）。
--   data 是**不透明的** JSON —— 内核不解释密码/令牌, 那是凭据插件的事。
--   一个可登录节点可以有多种方式（多行）; 删节点时级联清掉。
CREATE TABLE auth_methods (
	id         INTEGER PRIMARY KEY,
	type       TEXT    NOT NULL,   -- 认证类型名（= nodes.type; 站点可有多个 auth 类型）
	node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	method     TEXT    NOT NULL,   -- 登录方式名
	identifier TEXT    NOT NULL,   -- 登录标识
	data       TEXT    NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	-- 标识在**类型内**唯一: 不同 auth 类型各自有命名空间
	--（同一个邮箱既可以是 member 也可以是 staff）
	UNIQUE (type, method, identifier)
);
CREATE INDEX idx_auth_methods_node ON auth_methods(node_id);

-- 会话: 一个 realm 绑定的登录态。
--   只存 token 的 SHA-256 —— 库被读走也拿不到可直接用的令牌。
--   realm 是**不透明字符串**（内核不解释它): "哪一套登录态"是站点的事。
CREATE TABLE sessions (
	token_hash TEXT PRIMARY KEY,
	realm      TEXT    NOT NULL,
	node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX idx_sessions_node ON sessions(node_id);
CREATE INDEX idx_sessions_realm_node ON sessions(realm, node_id);

-- +goose Down
DROP TABLE sessions;
DROP TABLE auth_methods;
