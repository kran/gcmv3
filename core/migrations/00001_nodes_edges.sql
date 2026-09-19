-- +goose Up

-- 节点: 一切实体（内容 / 分类 / 人 / …），type 区分。
--   fields = 类型的**标量**字段（JSON）; 引用不进这里 —— 引用是边。
--   revision = 乐观锁版本（客户端读到几, 写回就必须是几）。
CREATE TABLE nodes (
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
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX idx_nodes_type ON nodes(type);
-- 地址空间是**全表**的: 有地址的节点共享一个命名空间（跨类型也不许撞）,
-- 于是 GetNode(address) 一条查询就能定位, 不可能歧义。
--
-- 索引不能带谓词: 实测（2 万行 + ANALYZE）只要谓词里有 type IN (...), planner 就
-- 再也不使用它（除非查询逐字复现那个列表 —— 站点查询不会）。
CREATE UNIQUE INDEX idx_nodes_address ON nodes(address);
-- 列表默认序（updated_at DESC, id DESC）走这条
CREATE INDEX idx_nodes_type_updated ON nodes(type, updated_at DESC, id DESC);

-- 边: 无身份的引用（类型系统只见"引用字段", 不见边）。**有向**:
--   A 引用 B 只有一条 (A, field, B); 反向是 B 的字段, 是另一条边。
CREATE TABLE edges (
	id         INTEGER PRIMARY KEY,
	from_node  INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	field      TEXT    NOT NULL,
	to_node    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	sort       INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	UNIQUE (from_node, field, to_node)
);
CREATE INDEX idx_edges_from ON edges(from_node, field);
CREATE INDEX idx_edges_from_sort ON edges(from_node, field, sort, id);
CREATE INDEX idx_edges_to ON edges(to_node, field);

-- +goose Down
DROP TABLE edges;
DROP TABLE nodes;
