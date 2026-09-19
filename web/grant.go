// Grant 一次请求的**角色 → 字段**映射 —— 同一容器, 两种用途, 两种默认:
//
//	读侧: 命中的字段 = 对这次读取**隐藏**（空 = 什么都不隐藏）
//	写侧: 命中的字段 = **可写**（空 = 没有授权这个动作 ⇒ 403）; "*" = 全部字段
//
// 求值是**并集**: actor 持有的任一角色授予了该字段（或 "*"）即命中 —— 兼任多个角色
// 只会更多权限, 不会互相抵消（不做 Deny 的回报: 没有优先级地狱）。
//
// 写侧预置 owner 的 "*"（见 newWriteGrant）—— owner 不是求值器里的特例, 只是"初始化时
// 被授予了全部"; 于是"owner 也受规则约束"是自然结果（规则可以用 Reset 把它收窄）。
package web

import (
	"sort"

	"github.com/kran/gcmv3/types"
)

type Grant struct {
	perms map[string]map[string]bool
}

// 全部字段的通配符。
const grantAll = "*"

func newGrant() *Grant {
	return &Grant{perms: map[string]map[string]bool{}}
}

// newWriteGrant 写侧初始化: owner 通配 "*"（内核统一给, 站点不用写、也不会忘）。
func newWriteGrant() *Grant {
	g := newGrant()
	g.Add(types.RoleOwner, grantAll)
	return g
}

// Add 给角色追加字段名（并集: 多个角色、多次调用都只会更多）。
func (g *Grant) Add(role string, names ...string) {
	if role == "" || len(names) == 0 {
		return
	}
	set, ok := g.perms[role]
	if !ok {
		set = map[string]bool{}
		g.perms[role] = set
	}
	for _, name := range names {
		set[name] = true
	}
}

// Reset 清空全部（写侧: 之后自己重新 Add; 读侧: 什么都不隐藏）。
//
// 注意规则可能被"整体重设"的意图误用: 一个类型只有一个规则回调, 所以没有别的回调
// 会被它丢掉 —— 但一个规则里多次 Reset 仍是自找麻烦。
func (g *Grant) Reset() { g.perms = map[string]map[string]bool{} }

// Has 这些角色里是否**任意一个**命中了该字段。
func (g *Grant) Has(roles []string, name string) bool {
	for _, role := range roles {
		set, ok := g.perms[role]
		if !ok {
			continue
		}
		if set[grantAll] || set[name] {
			return true
		}
	}
	return false
}

// anyFor actor 的角色里有任何一个被授予了东西（哪怕只是 "*"）。
//
// 写侧用它判"规则到底授权了没有": 站点规则什么都不给时, 只有 owner（初始化带 "*"）能过。
func (g *Grant) anyFor(roles []string) bool {
	for _, role := range roles {
		if len(g.perms[role]) > 0 {
			return true
		}
	}
	return false
}

// roles 容器里出现过的角色名（排序输出）—— 求值前用它校验拼写:
// 角色名打错 = 本该隐藏的没隐藏 = 泄漏, 所以 fail-loud。
func (g *Grant) roles() []string {
	out := make([]string, 0, len(g.perms))
	for role := range g.perms {
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

// names 容器里出现过的字段名（不含 "*"）—— 同样用于校验拼写。
func (g *Grant) names() []string {
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, set := range g.perms {
		for name := range set {
			if name == grantAll || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
