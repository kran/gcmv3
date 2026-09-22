// legacy-migrate 把 **v2 格式**的库搬成 gcmv3 格式（一次性工具, 可重复执行）。
//
//	go run ./tools/legacy-migrate -from gcm.sqlite.v2 -to gcm.sqlite.new -basedir .
//
// 三条约定:
//
//	源库**只读**打开（不会碰你的老数据）
//	目标必须是**新文件**（已存在就拒绝, 除非 -force）—— 宁可让你显式删,
//	也不静默覆盖一份可能还有用的库
//	搬完自己核对行数: 每张表源/目标不一致就报错退出（不留"搬了一半"没人知道）
//
// 搬什么、丢什么, 见 README 或下面的 copy* 函数注释。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kran/dba"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/types"
	"github.com/kran/gcmv3/web"
)

func main() {
	from := flag.String("from", "", "v2 格式的源库路径（只读）")
	to := flag.String("to", "", "目标新库路径（必须是新文件）")
	basedir := flag.String("basedir", ".", "站点目录（读 site.yaml 与 migrations/ demo/ 文件名）")
	force := flag.Bool("force", false, "允许覆盖已存在的目标文件")
	flag.Parse()
	if *from == "" || *to == "" {
		log.Fatal("用法: legacy-migrate -from <v2 库> -to <新库> [-basedir .] [-force]")
	}
	if _, err := os.Stat(*to); err == nil && !*force {
		log.Fatalf("%s 已存在 —— 换个名字, 或加 -force 明确覆盖", *to)
	}
	if err := run(*from, *to, *basedir); err != nil {
		log.Fatal(err)
	}
}

func run(from, to, basedir string) error {
	ts, err := loadTypes(filepath.Join(basedir, "site.yaml"))
	if err != nil {
		return err
	}
	source, err := dba.Open("sqlite", "file:"+from+"?mode=ro")
	if err != nil {
		return fmt.Errorf("打开源库（只读）: %w", err)
	}
	defer source.Close()
	target, err := dba.Open("sqlite", to)
	if err != nil {
		return fmt.Errorf("打开目标库: %w", err)
	}
	defer target.Close()

	// 目标库: 引擎的基础 schema（建表 + 索引）。
	err = target.Transaction(func(tx *dba.SQL) error {
		_, err := tx.Add(core.Schema()).Exec()
		return err
	})
	if err != nil {
		return fmt.Errorf("建目标库 schema: %w", err)
	}

	migrator := &migrator{source: source, target: target, types: ts}
	steps := []struct {
		name string
		run  func() (int, int, error) // (源行数, 目标行数)
	}{
		{"nodes", migrator.copyNodes},
		{"edges", migrator.copyEdges},
		{"auth_methods", migrator.copyAuthMethods},
		{"sessions", migrator.copySessions},
	}
	for _, step := range steps {
		before, after, err := step.run()
		if err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
		if before != after {
			return fmt.Errorf("%s: 源 %d 行, 目标 %d 行 —— 不一致, 中止", step.name, before, after)
		}
		fmt.Printf("  %-14s %d 行 ✓\n", step.name, after)
	}
	// 丢掉的东西要说清（免得以为"搬完了就一模一样"）。
	fmt.Println("  丢掉: accounts（v2 的遗留后台账号, 已被 auth_methods 取代）、" +
		"settings（空的）、nodes_fts*（内核没有全文检索）")
	if err := verifyTimeFields(target, ts); err != nil {
		return err
	}
	if err := markMigrations(target, basedir); err != nil {
		return err
	}
	fmt.Printf("完成: %s\n", to)
	return nil
}

type migrator struct {
	source *dba.SQL
	target *dba.SQL
	types  *types.Types
}

type sourceNode struct {
	ID        int64  `db:"id"`
	Type      string `db:"type"`
	Display   string `db:"display"`
	Fields    string `db:"fields"`
	CreatedAt string `db:"created_at"`
	UpdatedAt string `db:"updated_at"`
	Revision  int64  `db:"revision"`
}

// copyNodes v2 的 nodes → gcmv3 的 nodes:
//
//	display（系统列）→ 字段 name（gcmv3 没有 display; 名字是站点自己的字段）
//	字段里的 slug    → address（addressable 能力注入的字段; 不是 addressable 的类型
//	                   会打印警告并保留原样 —— 那说明 site.yaml 没开能力）
//	created_at/updated_at: ISO 字符串 → Unix 秒
func (m *migrator) copyNodes() (int, int, error) {
	rows, err := m.source.Add(`SELECT id, type, display, fields,
		created_at, updated_at, revision FROM nodes`).FetchList[sourceNode]()
	if err != nil {
		return 0, 0, err
	}
	count := 0
	for _, row := range rows {
		fields := map[string]any{}
		if strings.TrimSpace(row.Fields) != "" {
			if err := json.Unmarshal([]byte(row.Fields), &fields); err != nil {
				return count, count, fmt.Errorf("node #%d 字段不是 JSON: %w", row.ID, err)
			}
		}
		// display 是 v2 的**系统列**（节点的名字）—— gcmv3 里名字是字段, 搬过来。
		if name, _ := fields["name"].(string); row.Display != "" && name != row.Display {
			if name != "" {
				fmt.Printf("  · node #%d display=%q 覆盖了字段里的 name=%q\n", row.ID, row.Display, name)
			}
			fields["name"] = row.Display
		}
		if slug, ok := fields["slug"].(string); ok {
			delete(fields, "slug")
			if m.types.Addressable(row.Type) {
				fields["address"] = slug
			} else {
				fmt.Printf("  ! node #%d（%s）有 slug 但类型没开 addressable —— 该字段丢了\n", row.ID, row.Type)
			}
		}
		// **时间字段**也要转（v2 的 timestamp 字段是 ISO 字符串, gcmv3 是 Unix 秒）——
		// 只转列不转字段是这版工具第一遍的真 bug: 列对了、字段还是字符串,
		// 时间比较会变成 "TEXT vs INTEGER"（SQLite 里 TEXT 恒大于 INTEGER）, 静默错。
		if err := m.convertTimeFields(row.Type, row.ID, fields); err != nil {
			return count, count, err
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			return count, count, err
		}
		_, err = m.target.Insert("nodes", map[string]any{
			"id": row.ID, "type": row.Type, "revision": row.Revision,
			"fields":     string(encoded),
			"created_at": mustUnix(row.CreatedAt, row.ID),
			"updated_at": mustUnix(row.UpdatedAt, row.ID),
		}).Exec()
		if err != nil {
			return count, count, fmt.Errorf("node #%d: %w", row.ID, err)
		}
		count++
	}
	return len(rows), count, nil
}

// convertTimeFields 把该类型声明的 timestamp 字段值转成 Unix 秒（字符串 ISO → 整数）。
func (m *migrator) convertTimeFields(typeName string, id int64, fields map[string]any) error {
	def, ok := m.types.Type(typeName)
	if !ok {
		fmt.Printf("  ! node #%d 的类型 %q 不在 site.yaml 里 —— 字段原样搬\n", id, typeName)
		return nil
	}
	for _, field := range def.Fields {
		if field.Kind != types.KindTimestamp {
			continue
		}
		switch value := fields[field.Name].(type) {
		case nil:
		case string:
			fields[field.Name] = mustUnix(value, id)
		case float64:
			fields[field.Name] = int64(value)
		case int64:
		default:
			return fmt.Errorf("node #%d 的 %s 不是时间值: %T", id, field.Name, value)
		}
	}
	return nil
}

type sourceEdge struct {
	FromNode  int64  `db:"from_node"`
	Field     string `db:"field"`
	ToNode    int64  `db:"to_node"`
	Sort      int64  `db:"sort"`
	CreatedAt string `db:"created_at"`
}

// copyEdges v2 的 edges → gcmv3 的 edges: 去掉 v2 专有的 single_ref / symmetric 两列
// （gcmv3 的边只有一条约束: (from_node, field, to_node) 唯一）, 时间转 Unix 秒。
func (m *migrator) copyEdges() (int, int, error) {
	rows, err := m.source.Add(`SELECT from_node, field, to_node, sort, created_at
		FROM edges`).FetchList[sourceEdge]()
	if err != nil {
		return 0, 0, err
	}
	count := 0
	for _, row := range rows {
		_, err := m.target.Insert("edges", map[string]any{
			"from_node": row.FromNode, "field": row.Field, "to_node": row.ToNode,
			"sort": row.Sort, "created_at": mustUnix(row.CreatedAt, row.FromNode),
		}).Exec()
		if err != nil {
			return count, count, err
		}
		count++
	}
	return len(rows), count, nil
}

type sourceAuth struct {
	ID         int64  `db:"id"`
	Type       string `db:"type"`
	NodeID     int64  `db:"node_id"`
	Method     string `db:"method"`
	Identifier string `db:"identifier"`
	Data       string `db:"data"`
	CreatedAt  string `db:"created_at"`
	UpdatedAt  string `db:"updated_at"`
}

// copyAuthMethods 凭据**原样搬**: data 是不透明的, 而 v2 与 gcmv3 的内置口令都是
// bcrypt ⇒ 老密码照样能登（用户不用重设）。
func (m *migrator) copyAuthMethods() (int, int, error) {
	rows, err := m.source.Add(`SELECT id, type, node_id, method, identifier, data,
		created_at, updated_at FROM auth_methods`).FetchList[sourceAuth]()
	if err != nil {
		return 0, 0, err
	}
	count := 0
	for _, row := range rows {
		data := row.Data
		if strings.TrimSpace(data) == "" {
			data = "{}"
		}
		_, err := m.target.Insert("auth_methods", map[string]any{
			"id": row.ID, "type": row.Type, "node_id": row.NodeID,
			"method": row.Method, "identifier": row.Identifier, "data": data,
			"created_at": mustUnix(row.CreatedAt, row.ID),
			"updated_at": mustUnix(row.UpdatedAt, row.ID),
		}).Exec()
		if err != nil {
			return count, count, err
		}
		count++
	}
	return len(rows), count, nil
}

type sourceSession struct {
	TokenHash string `db:"token_hash"`
	Realm     string `db:"realm"`
	NodeID    int64  `db:"node_id"`
	ExpiresAt string `db:"expires_at"`
	CreatedAt string `db:"created_at"`
}

// copySessions 会话照搬（时间转 Unix 秒）—— 过期的不搬（引擎会拒, 搬了也是垃圾）:
// 站点已登录的浏览器不会因为换库就被踢, 过期的那些则自然清掉。
func (m *migrator) copySessions() (int, int, error) {
	rows, err := m.source.Add(`SELECT token_hash, realm, node_id, expires_at, created_at
		FROM sessions`).FetchList[sourceSession]()
	if err != nil {
		return 0, 0, err
	}
	now := time.Now().Unix()
	count := 0
	for _, row := range rows {
		if mustUnix(row.ExpiresAt, 0) <= now {
			continue
		}
		_, err := m.target.Insert("sessions", map[string]any{
			"token_hash": row.TokenHash, "realm": row.Realm, "node_id": row.NodeID,
			"expires_at": mustUnix(row.ExpiresAt, 0), "created_at": mustUnix(row.CreatedAt, 0),
		}).Exec()
		if err != nil {
			return count, count, err
		}
		count++
	}
	// 过期的在目标里不出现 ⇒ 行数天然不等 ✗ —— 这里返回"源里有效的条数"作为期望值。
	valid := 0
	for _, row := range rows {
		if mustUnix(row.ExpiresAt, 0) > now {
			valid++
		}
	}
	return valid, count, nil
}

// markMigrations 把当前的 migrations/*.sql 与 demo/*.sql 在目标库里标成"已执行"。
//
// 必须做: 这些文件用**显式 id** 插入（seed 数据）, 而老库里已经有那些行了 ——
// 不标就会在站点启动时重跑一遍, 主键冲突。
func markMigrations(target *dba.SQL, basedir string) error {
	for _, set := range []struct{ dir, table string }{
		{"migrations", "migrations"},
		{"demo", "migrations_demo"},
	} {
		names, err := sqlFiles(filepath.Join(basedir, set.dir))
		if err != nil {
			return err
		}
		if len(names) == 0 {
			continue
		}
		_, err = target.Add(`CREATE TABLE IF NOT EXISTS ` + set.table +
			` (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`).Exec()
		if err != nil {
			return err
		}
		for _, name := range names {
			_, err = target.Insert(set.table, map[string]any{
				"name": name, "applied_at": time.Now().Unix(),
			}).Exec()
			if err != nil {
				return fmt.Errorf("标记 %s/%s: %w", set.dir, name, err)
			}
		}
		fmt.Printf("  记账: %-18s %d 个迁移标成已执行\n", set.table, len(names))
	}
	return nil
}

// verifyTimeFields 搬完自己核一遍: 每个 timestamp 字段的值必须是整数秒, 且量纲是秒。
//
// 与站点测试里那条"canonical 时间"断言同一件事 —— 手写 SQL 的迁移绕过了内核校验,
// 单位写错不会报错, 只会静默给出错结果。
func verifyTimeFields(db *dba.SQL, ts *types.Types) error {
	for _, typeName := range ts.Names() {
		def, _ := ts.Type(typeName)
		for _, field := range def.Fields {
			if field.Kind != types.KindTimestamp {
				continue
			}
			path := "$." + field.Name
			bad, err := db.Add(`SELECT COUNT(*) FROM nodes WHERE type = #{1}
				AND json_extract(fields, #{2}) IS NOT NULL
				AND (typeof(json_extract(fields, #{2})) <> 'integer'
				  OR json_extract(fields, #{2}) > 100000000000)`,
				typeName, path).FetchOne[int]()
			if err != nil {
				return err
			}
			if bad != nil && *bad > 0 {
				return fmt.Errorf("%s.%s 有 %d 个值不是整数秒（时间字段必须是 Unix 秒, 不是毫秒）",
					typeName, field.Name, *bad)
			}
		}
	}
	fmt.Println("  时间字段自检 ✓（全是整数秒）")
	return nil
}

func sqlFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// loadTypes 读站点声明（site.yaml: name + types）—— 用框架的同一个加载器,
// 校验口径与起站完全一致（不再自己解析一遍）。
func loadTypes(path string) (*types.Types, error) {
	decl, err := web.LoadSiteFile(path)
	if err != nil {
		return nil, err
	}
	return decl.Types, nil
}

// mustUnix 把 v2 的时间列（ISO 字符串）转成 Unix 秒。
//
// v2 的列是字符串、gcmv3 是整数 —— 这**必须**转, 否则时间比较会变成
// "TEXT vs INTEGER"（SQLite 里 TEXT 恒大于 INTEGER）, 静默给出错结果。
func mustUnix(raw string, id int64) int64 {
	for _, layout := range []string{
		"2006-01-02T15:04:05Z",
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999999-07:00",
	} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC().Unix()
		}
	}
	if raw == "" {
		return 0
	}
	fmt.Printf("  ! 时间解析不了（#%d）: %q —— 按 0 处理\n", id, raw)
	return 0
}
