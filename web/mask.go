// 字段级可见性（读规则的第二个出参）的应用。
//
// 只做三件事: 拷贝、删键、按子节点自己的类型递归处理 Expand。
//
// 约定:
//   - hide 是顶层字段名; 命中即从 JSON 里消失（不是置空）。
//   - 空 = 全部字段可见。
//   - **只影响输出**, 不影响查询能力（排序/筛选照旧, 行集大小不变）。于是理论上能靠
//     `prefix(phone, "1")` 这类条件探测被藏起来的值 —— 这是接受的取舍（要堵就得把
//     隐藏字段也从可筛选集合里拿掉, 那要另一套机制）; 换来的是一致性: 掩码与查询
//     解耦, 同一个读规则不会因为"换了个排序"而行为不同。
//   - 只读: 输入节点永不被就地修改; 需要裁剪时返回拷贝, 不需要时原样返回。
package web

import "github.com/kran/gcmv3/core"

// MaskNode 返回裁剪后的节点。hide 为空时原样返回（不拷贝, 零分配）。
//
// 调用方拿返回值继续用即可: 既不要假设原节点被改, 也不要依赖返回值 != 入参。
func (c *CmsCtx) MaskNode(node *core.Node) (*core.Node, error) {
	if node == nil {
		return nil, nil
	}
	_, hidden, err := c.readRule(node.Type)
	if err != nil {
		return nil, err
	}
	expand, expandChanged, err := c.maskExpand(node.Expand)
	if err != nil {
		return nil, err
	}
	if len(hidden) == 0 && !expandChanged {
		return node, nil
	}
	out := *node
	if len(hidden) > 0 {
		fields := make(map[string]any, len(node.Fields))
		for name, value := range node.Fields {
			fields[name] = value
		}
		for _, name := range hidden {
			delete(fields, name)
		}
		out.Fields = fields
	}
	if expandChanged {
		out.Expand = expand
	}
	return &out, nil
}

// MaskNodes 就地裁剪（每个节点按自己的类型解析规则; 指针不变, 变的是它指向的节点）。
//
// 站点 handler 输出一批节点时用它; 框架自己的读入口已经自动套过。
func (c *CmsCtx) MaskNodes(nodes []*core.Node) error {
	for _, node := range nodes {
		masked, err := c.MaskNode(node)
		if err != nil {
			return err
		}
		if masked != node {
			*node = *masked
		}
	}
	return nil
}

// maskExpand 裁剪展开容器。只有真的改了子节点才返回新 map（changed = true）。
//
// 目标类型对这个读取没有规则（或规则拒绝）⇒ **整条引用不下发**（fail-closed）:
// 不因为展开而在响应里漏出不该看的节点, 也不该让整个读请求失败。
func (c *CmsCtx) maskExpand(in map[string]any) (map[string]any, bool, error) {
	if len(in) == 0 {
		return in, false, nil
	}
	out, changed := in, false
	// detach 第一次真要改时把 map 拷一份（不改入参）
	detach := func() {
		if changed {
			return
		}
		clone := make(map[string]any, len(in))
		for key, value := range in {
			clone[key] = value
		}
		out, changed = clone, true
	}
	for key, value := range in {
		switch typed := value.(type) {
		case *core.Node:
			masked, err := c.MaskNode(typed)
			if err != nil {
				// 目标不可读 ⇒ 整条引用不下发
				detach()
				delete(out, key)
				continue
			}
			if masked == typed {
				continue
			}
			detach()
			out[key] = masked
		case []*core.Node:
			list, copied, dropped := typed, false, false
			for i, child := range typed {
				masked, err := c.MaskNode(child)
				if err != nil {
					dropped = true
					break
				}
				if masked == child {
					continue
				}
				if !copied {
					list = append([]*core.Node(nil), typed...)
					copied = true
				}
				list[i] = masked
			}
			switch {
			case dropped:
				detach()
				delete(out, key)
			case copied:
				detach()
				out[key] = list
			}
		}
	}
	return out, changed, nil
}
