package web

import (
	"testing"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// 隐藏字段从 JSON 里消失（不是置空）; 没要裁的东西原样返回（同一个指针）。
func TestMaskNodeHidesFields(t *testing.T) {
	site := newPolicySite(t)
	site.Type("article").OnRead(func(c *CmsCtx, where *so.Where, hide *Grant) error {
		*where = so.P("true")
		if c.Actor().IsAnonymous() {
			hide.Add(types.RolePublic, "phone")
		}
		return nil
	})
	cms, _ := ctxFor(site)
	node := &core.Node{ID: 1, Type: "article", Fields: core.Fields{"title": "甲", "phone": "138"}}
	masked, err := cms.MaskNode(node)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := masked.Fields["phone"]; ok {
		t.Fatalf("phone 该消失: %#v", masked.Fields)
	}
	if masked.Fields["title"] != "甲" {
		t.Fatalf("别的字段该在: %#v", masked.Fields)
	}
	if _, ok := node.Fields["phone"]; !ok {
		t.Fatal("不该改入参")
	}
}

func TestMaskNodeNoHideReturnsSamePointer(t *testing.T) {
	site := newPolicySite(t)
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	cms, _ := ctxFor(site)
	node := &core.Node{ID: 1, Type: "article", Fields: core.Fields{"title": "甲"}}
	masked, err := cms.MaskNode(node)
	if err != nil {
		t.Fatal(err)
	}
	if masked != node {
		t.Fatal("没有要裁的东西时该原样返回（零分配）")
	}
}

// 展开的子节点按**它自己的类型**递归掩码。
func TestMaskExpandRecurses(t *testing.T) {
	site := newPolicySite(t)
	openAll := func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	}
	site.Type("article").OnRead(openAll)
	site.Type("member").OnRead(func(c *CmsCtx, where *so.Where, hide *Grant) error {
		*where = so.P("true")
		if !c.Actor().IsOwner() {
			hide.Add(types.RolePublic, "phone")
		}
		return nil
	})
	cms, _ := ctxFor(site)

	author, err := site.Engine().CreateNode(nil, &core.Node{Type: "member",
		Fields: core.Fields{"name": "甲", "phone": "138"}})
	if err != nil {
		t.Fatal(err)
	}
	article, err := site.Engine().CreateNode(nil, &core.Node{Type: "article",
		Fields: core.Fields{"title": "文", "author": author}})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := site.Engine().GetNode(article)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := site.Engine().ExpandNodes([]*core.Node{loaded}, "author")
	if err != nil {
		t.Fatal(err)
	}
	masked, err := cms.MaskNode(nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	child, ok := masked.Expand["author"].(*core.Node)
	if !ok {
		t.Fatalf("展开该还在: %#v", masked.Expand)
	}
	if _, ok := child.Fields["phone"]; ok {
		t.Fatalf("子节点该按自己的类型掩码: %#v", child.Fields)
	}
	if child.Fields["name"] != "甲" {
		t.Fatalf("子节点的其它字段该在: %#v", child.Fields)
	}
	// 原节点（含原展开）不该被改
	original, _ := nodes[0].Expand["author"].(*core.Node)
	if _, ok := original.Fields["phone"]; !ok {
		t.Fatal("不该改入参的展开子节点")
	}
}

// 目标类型不可读（没注册读规则）⇒ 整条引用不下发（fail-closed）, 但不影响别的引用,
// 也不让整个读失败。
func TestMaskExpandDropsUnreadableRef(t *testing.T) {
	site := newPolicySite(t)
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	// member 故意不注册读规则
	cms, _ := ctxFor(site)

	author, err := site.Engine().CreateNode(nil, &core.Node{Type: "member",
		Fields: core.Fields{"name": "甲"}})
	if err != nil {
		t.Fatal(err)
	}
	article, err := site.Engine().CreateNode(nil, &core.Node{Type: "article",
		Fields: core.Fields{"title": "文", "author": author}})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := site.Engine().GetNode(article)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := site.Engine().ExpandNodes([]*core.Node{loaded}, "author")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes[0].Expand) == 0 {
		t.Skip("这个 fixture 没展开出东西")
	}
	masked, err := cms.MaskNode(nodes[0])
	if err != nil {
		t.Fatalf("展开不可读不该让整个读失败: %v", err)
	}
	if _, ok := masked.Expand["author"]; ok {
		t.Fatalf("不可读的引用该整条不下发: %#v", masked.Expand)
	}
	if masked.Fields["title"] != "文" {
		t.Fatalf("节点本身该照常: %#v", masked.Fields)
	}
}

// MaskNodes 就地裁剪切片元素。
func TestMaskNodesInPlace(t *testing.T) {
	site := newPolicySite(t)
	site.Type("article").OnRead(func(c *CmsCtx, where *so.Where, hide *Grant) error {
		*where = so.P("true")
		if c.Actor().IsAnonymous() {
			hide.Add(types.RolePublic, "phone")
		}
		return nil
	})
	cms, _ := ctxFor(site)
	nodes := []*core.Node{
		{ID: 1, Type: "article", Fields: core.Fields{"title": "甲", "phone": "1"}},
		{ID: 2, Type: "article", Fields: core.Fields{"title": "乙", "phone": "2"}},
	}
	err := cms.MaskNodes(nodes)
	if err != nil {
		t.Fatal(err)
	}
	for i, node := range nodes {
		if _, ok := node.Fields["phone"]; ok {
			t.Fatalf("nodes[%d] 该被裁: %#v", i, node.Fields)
		}
		if node.Fields["title"] == "" {
			t.Fatalf("nodes[%d] 的别的字段该在: %#v", i, node.Fields)
		}
	}
}
