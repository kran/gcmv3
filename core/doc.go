// Package core 内核：节点 / 边 / 认证 / 钩子 / 类型系统 / 结构化查询。
//
// 边界（判据：能卸掉且不影响内核 CRUD ⇒ 插件；只给界面看的 ⇒ web；只有内核读写要它 ⇒ core）:
//
//	node     节点 CRUD 与读投影
//	edge     引用边与图原语
//	auth     opaque 凭据 + realm 绑定会话
//	hook     生命周期事件总线（广播；多订阅，可带事务）
//	schema   类型系统（types 包承载）
//	query    结构化查询（AST → SQL）
//
// 不进这里：全文检索（插件）· 树视图（web）· 行范围与字段掩码（授权词汇，web）
// · 通用 KV 配置（承载层）· 只能预览不能执行的半成品（不做）。
package core
