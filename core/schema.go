// 引擎的**基础** schema —— 建表 + 索引, 没有增量更新。
//
// 引擎自己不执行它（不做启动期 DDL, 也不做迁移版本管理）: 使用方拿
// `core.Schema()` 直接执行（多条语句一次 Exec 即可, 每条都 IF NOT EXISTS ⇒ 幂等）。
package core

import _ "embed"

//go:embed schema.sql
var schemaSQL string

// Schema 引擎带的那几张表的 DDL（站点自己的表归站点自己建）。
func Schema() string { return schemaSQL }
