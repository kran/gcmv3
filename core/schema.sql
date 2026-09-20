-- 引擎的**基础** schema（建表 + 索引, 没有增量更新）。
--
-- 由使用方**直接执行**（`core.Schema()` 给出这份文本; 多条语句一次 Exec 即可,
-- SQLite 的 driver 支持; 每条都 IF NOT EXISTS ⇒ 幂等, 每次启动跑一遍无害）。
--
-- 这里是引擎带的那几张表; 站点自己的表归站点自己建。索引跟着表一起在这里声明 ——
-- 引擎不做启动期 DDL 推导（那是"schema→DDL"的魔法, 已经去掉）。

-- 节点: 一切实体（内容 / 分类 / 人 / …），type 区分。
--   fields = 类型的**标量**字段（JSON）; 引用不进这里 —— 引用是边。
--   revision = 乐观锁版本（客户端读到几, 写回就必须是几）。
CREATE TABLE IF NOT EXISTS nodes (
	id         INTEGER PRIMARY KEY,
	type       TEXT    NOT NULL,
	revision   INTEGER NOT NULL DEFAULT 1,
	fields     TEXT    NOT NULL DEFAULT '{}',
	-- address 是**生成列**: 从注入的 address 字段投影出来, 由 DB 自己算 ——
	-- 写路径零代码、不可能与来源漂移、VIRTUAL 不需要回填。
	--
	-- NULLIF(..., '') 是必须的: 显式传空串会存成 ''（不是 NULL）, 于是两条"没有
	-- 地址"的节点会互相撞车。压回 NULL 之后, "没有地址"的行天然不参与唯一
	-- （SQLite 里 NULL 互不相等）。
	--
	-- 没有 address 字段的类型（= 没声明 addressable）算出来是 NULL ⇒ 选择性参与
	-- 是免费的, 而且**不需要把类型名写进 DDL** ⇒ 改配置不用重建表。
	address    TEXT GENERATED ALWAYS AS (NULLIF(json_extract(fields, '$.address'), '')) VIRTUAL,
	-- uniq 是**类型内唯一键**（声明 capabilities.unique 的那一类才有值）。
	--
	-- 与 address 的关键区别: 它**不是生成列** —— 因为键里的字段名每个类型都不同
	-- （member 是 credit_code, 任职是 person+company）, 而生成列的表达式写死在 DDL 里。
	-- 所以由**写路径**算好写进来（core/uniq.go 的 projectUniq）:
	--
	--	["position","12","34"]   引用按目标 id 进键; 任一部分为空 ⇒ 整键 NULL
	--
	-- 值是 JSON 数组（不是分隔符拼串）: 值里带分隔符也不会撞（member/a:b vs member:a/b),
	-- 而且冲突报错时能原样打印出来给人看。NULL ⇒ 不参与唯一（与 address 同语义）。
	uniq       TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_nodes_type ON nodes(type);
-- 列表默认序（updated_at DESC, id DESC）走这条
CREATE INDEX IF NOT EXISTS idx_nodes_type_updated ON nodes(type, updated_at DESC, id DESC);
-- 地址空间是**全表**的: 有地址的节点共享一个命名空间（跨类型也不许撞）,
-- 于是 GetNode(address) 一条查询就能定位, 不可能歧义。
--
-- 索引不能带谓词: 实测（2 万行 + ANALYZE）只要谓词里有 type IN (...), planner 就
-- 再也不使用它（除非查询逐字复现那个列表 —— 站点查询不会）。
CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_address ON nodes(address);
-- 类型内唯一: 一个索引覆盖所有类型（类型名是键的第一个元素）——
-- 于是"一个类型几个唯一字段"不用改 DDL, 也不用把类型名写进语句里。
CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_uniq ON nodes(uniq);

-- 边: 无身份的引用（类型系统只见"引用字段", 不见边）。**有向**:
--   A 引用 B 只有一条 (A, field, B); 反向是 B 的字段, 是另一条边。
CREATE TABLE IF NOT EXISTS edges (
	id         INTEGER PRIMARY KEY,
	from_node  INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	field      TEXT    NOT NULL,
	to_node    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	sort       INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	-- 边唯一的约束只有这一条: 同一节点、同一字段、同一目标只存一条。
	--（"单个引用字段至多一条边"不在这里靠数据库强制 —— 那需要把 schema 推导成
	--  存储标志位再建部分唯一索引; 应用层写不出来两条, 并发有单写者 + 乐观锁挡着,
	--  真出现脏数据读的时候会响亮报错。）
	UNIQUE (from_node, field, to_node)
);
-- 出边按 (from_node, field) 取, 且要保 sort 序 ⇒ 这条索引一次覆盖过滤与排序。
-- 不再单建 (from_node, field): 它是上面 UNIQUE 索引的前缀, 纯写放大。
CREATE INDEX IF NOT EXISTS idx_edges_from_sort ON edges(from_node, field, sort, id);
-- 入边方向（入边展开、删除检查的"谁引用了我"）。
CREATE INDEX IF NOT EXISTS idx_edges_to ON edges(to_node, field);

-- 凭据: 一条 = 一个登录标识（email / phone / wechat / oauth sub …）。
--   data 是**不透明的** JSON —— 内核不解释密码/令牌, 那是凭据插件的事。
--   一个可登录节点可以有多种方式（多行）; 删节点时级联清掉。
CREATE TABLE IF NOT EXISTS auth_methods (
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
CREATE INDEX IF NOT EXISTS idx_auth_methods_node ON auth_methods(node_id);

-- 会话: 一个 realm 绑定的登录态。
--   只存 token 的 SHA-256 —— 库被读走也拿不到可直接用的令牌。
--   realm 是**不透明字符串**（内核不解释它): "哪一套登录态"是站点的事。
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	realm      TEXT    NOT NULL,
	node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_node ON sessions(node_id);
CREATE INDEX IF NOT EXISTS idx_sessions_realm_node ON sessions(realm, node_id);
