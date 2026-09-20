// 索引表与同步。
//
// 表结构: FTS5(type, text) —— rowid = 节点 id。
//
//	type  只是**过滤列**（bm25 权重给 0）: 检索按类型走, 不靠回读去筛
//	text  所有 QueryOps.Text 字段的值拼起来, 已 bigram 预分词
//
// 索引与节点**同一个事务**提交（事件带 tx）: 崩溃/回滚不会留下半个索引。
package search

import (
	"fmt"
	"strings"

	"github.com/kran/dba"
	"github.com/kran/gcmv3/core"
)

// createTable 建索引表（幂等 —— 已存在就什么都不做, 索引数据原样保留）。
func (p *Plugin) createTable() error {
	_, err := p.site.DB().Add(`CREATE VIRTUAL TABLE IF NOT EXISTS search_fts USING fts5(type, text)`).Exec()
	if err != nil {
		return fmt.Errorf("search: 建索引表: %w", err)
	}
	return nil
}

// syncNode 索引一个节点（upsert）。没配成可搜索类型 ⇒ 清掉残留（类型被移出配置时）。
func (p *Plugin) syncNode(tx *dba.SQL, node *core.Node) error {
	if node == nil {
		return nil
	}
	text := p.indexText(node)
	if text == "" {
		// 没有可索引内容: 不写空行（也清掉改配置后的残留）
		return p.deleteNode(tx, node.ID)
	}
	// FTS5 虚拟表不支持 UPSERT ⇒ 先删后插（同一事务, 原子）
	err := p.deleteNode(tx, node.ID)
	if err != nil {
		return err
	}
	_, err = tx.Add(`INSERT INTO search_fts(rowid, type, text) VALUES (#{1}, #{2}, #{3})`,
		node.ID, node.Type, bigram(text)).Exec()
	if err != nil {
		return fmt.Errorf("search: 写索引 #%d: %w", node.ID, err)
	}
	return nil
}

// deleteNode 删索引（节点被永久删除时）。
func (p *Plugin) deleteNode(tx *dba.SQL, nodeID int64) error {
	_, err := tx.Add(`DELETE FROM search_fts WHERE rowid = #{1}`, nodeID).Exec()
	if err != nil {
		return fmt.Errorf("search: 删索引 #%d: %w", nodeID, err)
	}
	return nil
}

// indexText 拼一个节点里**所有文本字段**的值（可搜索字段由类型声明决定:
// QueryOps.Text —— 见设计讨论, 不再单独维护一份"可搜索字段"清单）。
//
//	types 的声明就是唯一真相: text/textarea/richtext/address 这些 kind 的
//	QueryOps.Text 为 true ⇒ 它们的值进索引; 布尔/数字/引用不进（搜"1"没意义）。
//
// 拼在一起而不是分列: 权重差异（标题 > 正文）在 v3 里没有可靠声明来源
// （admin.display 是展示名, 不一定是"标题"）—— 与其猜, 不如一视同仁。
func (p *Plugin) indexText(node *core.Node) string {
	if !p.searchable(node.Type) {
		return ""
	}
	def, ok := p.site.Types().Type(node.Type)
	if !ok {
		return ""
	}
	var parts []string
	for _, field := range def.Fields {
		if !p.site.Types().FieldQueryOps(field).Text {
			continue
		}
		value, ok := node.Fields[field.Name].(string)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		parts = append(parts, value)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// searchable 该类型是否配成可搜索（Mount 时校验过类型存在）。
func (p *Plugin) searchable(typeName string) bool {
	return p.searchableType[typeName]
}
