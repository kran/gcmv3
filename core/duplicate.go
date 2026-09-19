package core

import (
	"errors"
	"fmt"
	"strings"

	sqlite "modernc.org/sqlite"
)

// ErrDuplicate 唯一约束冲突: 地址撞车、凭据标识重复、同一条边重复写入。
//
// 内核**只支持 SQLite**（AGENTS.md 的边界）, 所以这里按驱动的扩展结果码判定 ——
// modernc 的驱动连着库就打开了 extended_result_codes, 于是 2067 精确等于
// "UNIQUE constraint failed", 不用去匹配错误文案（文案会变, 码不会）。
//
// 包成领域错误之后, 上层就能给出 409 而不是 500（"这个地址/这个邮箱已经有了"是
// 客户端能处理的事实, 不是服务端故障）。
var ErrDuplicate = errors.New("core: duplicate")

// sqliteConstraintUnique = SQLITE_CONSTRAINT_UNIQUE（扩展结果码）。
const sqliteConstraintUnique = 2067

// duplicate 唯一约束错 ⇒ ErrDuplicate（别的错误原样返回）。
//
// 只在**写路径**上套一层。
func duplicate(err error) error {
	if err == nil || !isDuplicate(err) {
		return err
	}
	return fmt.Errorf("%w: %s", ErrDuplicate, duplicateDetail(err.Error()))
}

// isDuplicate 是不是唯一约束冲突。
func isDuplicate(err error) bool {
	var sqlErr *sqlite.Error
	if !errors.As(err, &sqlErr) {
		return false
	}
	return sqlErr.Code() == sqliteConstraintUnique
}

// duplicateDetail 从驱动文案里取撞到的约束目标（`nodes.address` 这种）。
//
// 取不到就退回原文案 —— 信息不全可以, 但绝不能为空。
func duplicateDetail(message string) string {
	_, after, found := strings.Cut(message, "UNIQUE constraint failed: ")
	if !found {
		return message
	}
	detail := strings.TrimSpace(after)
	// 去掉尾部的 " (2067)"
	if index := strings.LastIndex(detail, " ("); index > 0 {
		detail = detail[:index]
	}
	if detail == "" {
		return message
	}
	return detail
}
