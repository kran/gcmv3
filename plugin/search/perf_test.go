package search

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kran/dba"
	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/web"
)

// 检索性能（**站点的真实中文正文**, 放大到 N 条）—— 量插件实际跑的那几种 SQL:
//
//	OR 列表    searchRanked 的 OR 那一趟（"按匹配度返回"的主查询, ORDER BY rank,rowid LIMIT）
//	短语       searchRanked 的短语那一趟（同样形状, 只是 MATCH 是整句短语）
//	逐词探针   presentTokens（每个词元一次 MATCH —— 查询词越多, 探针次数越多）
//
// 另外量两件运维上真正要紧的: **索引体积**（bigram 大约是正文的几倍）与
// **全量重建（-reindex）的耗时**。
//
// 默认跳过（要插几千条, 会让普通 go test 变慢）; 要看就跑:
//
//	SEARCH_BENCH=1 go test ./plugin/search/ -run TestSearchPerformance -v
func TestSearchPerformance(t *testing.T) {
	if os.Getenv("SEARCH_BENCH") == "" {
		t.Skip("设 SEARCH_BENCH=1 才跑（会插入几千条数据）")
	}
	texts := benchTexts()
	for _, total := range []int{1000, 5000, 20000} {
		runSearchPerf(t, total, texts)
	}
}

// benchTexts 真实正文素材（viicn 站的文章正文, 取一段当模板再轮换拼接）。
//
// 用真中文而不是随机串: bigram 的分布、常见字的 idf 都由真实语料决定, 编不出来。
func benchTexts() []string {
	base := `<p>于微光中寻未来，于混沌中塑新生。由司南创新（北京）研究院、未来商业论坛组委会主办，振兴国际智库承办的论坛在京举行。与会专家围绕宏观经济形势、产业升级路径、区域协调发展、企业创新与品牌建设等议题展开研讨，并就政策落地、人才培养、科技成果转化等提出建议。会议指出，要坚持稳中求进，把握新一轮科技革命和产业变革机遇，推动高质量发展。</p>`
	// 造出**有差异**的正文（真实站点不是每篇都同一句话）: 每篇随机抽若干主题词拼进去。
	// 这样"常见词"只命中一部分文档, 才是真实的相关度分布（全同一句话 = 最坏情况,
	// 那种数据下 FTS5 要为每一行打分 ⇒ O(命中数)）。
	topics := []string{"经济", "产业", "论坛", "智库", "政策", "创新", "发展", "研究",
		"报告", "企业", "区域", "品牌", "人才", "科技", "金融", "文旅", "农业", "教育",
		"新能源", "数字化", "国际化", "营商环境", "乡村振兴", "绿色发展"}
	out := make([]string, 0, len(topics)*2)
	for i := range topics {
		picked := []string{}
		for j := 0; j < 3; j++ {
			picked = append(picked, topics[(i+j*7)%len(topics)])
		}
		out = append(out, base+"<p>本文聚焦"+strings.Join(picked, "、")+"等领域。</p>")
	}
	return out
}

func runSearchPerf(t *testing.T, total int, texts []string) {
	basedir := t.TempDir()
	err := os.WriteFile(basedir+"/site.yaml", []byte(`
name: 性能测试
types:
  article:
    capabilities: { addressable: true }
    fields:
      - { name: name, kind: text }
      - { name: body, kind: richtext }
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	site, err := web.Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = site.Close() }()
	site.Type("article").OnRead(func(_ *web.CmsCtx, where *so.Where, _ *web.Grant) error {
		*where = so.P("true")
		return nil
	})
	plugin, err := Mount(site, Options{Types: []string{"article"}})
	if err != nil {
		t.Fatal(err)
	}
	db := site.DB()

	// 建数据: 一个事务里插完（钩子照常触发 ⇒ 索引同步写在同一个事务里）
	engine := site.Engine()
	start := time.Now()
	err = db.Transaction(func(tx *dba.SQL) error {
		for i := 0; i < total; i++ {
			body := texts[i%len(texts)]
			_, err := engine.CreateNode(tx, &core.Node{Type: "article", Fields: core.Fields{
				"name": fmt.Sprintf("第 %d 篇：%s", i+1, string([]rune(body)[20:28])),
				"body": body,
			}})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	insertCost := time.Since(start)

	// 全量重建一次（-reindex 的成本）
	start = time.Now()
	err = plugin.Rebuild()
	if err != nil {
		t.Fatal(err)
	}
	rebuildCost := time.Since(start)

	indexed, err := db.Add(`SELECT COUNT(1) FROM search_fts`).FetchOne[int64]()
	if err != nil {
		t.Fatal(err)
	}
	sizeBefore := fileSize(basedir)
	_, err = db.Add(`VACUUM`).Exec()
	if err != nil {
		t.Fatal(err)
	}
	sizeAfter := fileSize(basedir)

	t.Logf("── %d 条（正文约 %d KB/篇, 索引 %d 行）", total, len(texts[0])/1024+1, *indexed)
	t.Logf("   写入(含索引, 单事务) %v | 全量重建 %v | 库 %d MB → VACUUM 后 %d MB",
		insertCost.Round(time.Millisecond), rebuildCost.Round(time.Millisecond),
		sizeBefore/1024/1024, sizeAfter/1024/1024)

	queries := []struct {
		label string
		q     string
	}{
		{"1 个词(常见)", "经济"},
		{"1 个词(稀有)", "新能源"},
		{"3 个词     ", "经济 论坛 发展"},
		{"10 个词    ", "中国 经济 发展 论坛 智库 研究 报告 产业 政策 创新"},
		{"长句(短语)  ", "振兴国际智库承办的论坛在京举行"},
	}
	for _, one := range queries {
		present, err := plugin.presentTokens(db, bigram(one.q))
		if err != nil {
			t.Fatal(err)
		}
		cost, hits := timeSearch(t, plugin, db, one.q, 20)
		t.Logf("   %s 探针 %2d 次 | searchRanked %8v（前 20 条命中 %d）",
			one.label, len(present), cost.Round(time.Microsecond), hits)
	}
}

// timeSearch 跑 20 次取"中位/最慢"（跑一次容易被 GC 影响）。
func timeSearch(t *testing.T, plugin *Plugin, db *dba.SQL, query string, window int) (time.Duration, int) {
	t.Helper()
	costs := make([]time.Duration, 0, 21)
	hits := 0
	for i := 0; i < 21; i++ {
		start := time.Now()
		ranked, _, err := plugin.searchRanked(db, query, "article", window)
		if err != nil {
			t.Fatal(err)
		}
		costs = append(costs, time.Since(start))
		hits = len(ranked)
	}
	sort.Slice(costs, func(i, j int) bool { return costs[i] < costs[j] })
	median := costs[len(costs)/2]
	if costs[len(costs)-1] > median*3 {
		return costs[len(costs)-1], hits // 最慢那次明显异常就报它（别粉饰）
	}
	return median, hits
}

func fileSize(basedir string) int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(basedir + "/gcm.sqlite" + suffix)
		if err == nil {
			total += info.Size()
		}
	}
	return total
}
