// 全量重建。
package search

import (
	"fmt"

	"github.com/kran/gcmv3/core"
)

// rebuildPage 重建时每批读多少节点（一次入内存太多没必要 —— 索引是逐条 upsert）。
const rebuildPage = 500

// Rebuild 全量重建索引（清空 + 按配置的类型重新索引）。
//
// 什么时候用:
//   - 换过可搜索字段/类型配置（Options.Types 变了, 或类型的字段声明变了）
//   - 索引表被手工改过 / 想确认"索引和库一致"
//
// 走**引擎**读节点（"系统自己要看"的可信调用, 不受读规则限制）—— 索引的是全量,
// 可见性在查询时按读规则裁（那是检索的一部分, 与索引无关）。
//
// 不在事务里跑: 几万条一次事务会长时间锁库。中途失败 ⇒ 索引不全, 重跑一次即可
// （Rebuild 是幂等的）。
func (p *Plugin) Rebuild() error {
	_, err := p.site.DB().Add(`DELETE FROM search_fts`).Exec()
	if err != nil {
		return fmt.Errorf("search: 清空索引: %w", err)
	}
	for typeName := range p.searchableType {
		offset := 0
		for {
			nodes, err := p.site.Engine().GetNodes(core.NodeQuery{Type: typeName}, rebuildPage, offset)
			if err != nil {
				return fmt.Errorf("search: 重建 %s: %w", typeName, err)
			}
			if len(nodes) == 0 {
				break
			}
			for _, node := range nodes {
				err = p.syncNode(p.site.DB(), node)
				if err != nil {
					return err
				}
			}
			if len(nodes) < rebuildPage {
				break
			}
			offset += len(nodes)
		}
	}
	return nil
}
