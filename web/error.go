// 业务错误 —— 拒绝、白名单、找不到, 都走它。
//
// 它只描述"是什么错"（状态码 + 码 + 人说的一句话 + 字段级细节）:
// **怎么写响应是路由层的事**（E 步统一映射）。core 的错误在这里翻译成 HTTP 语义。
package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kran/gcmv3/core"
)

// Error 一次请求里可预期地失败。
type Error struct {
	Status  int               // HTTP 状态码
	Code    string            // 机器可读的短码（默认由状态码推出）
	Message string            // 给人看的一句话
	Details map[string]string // 字段级细节（如白名单拒绝的字段名 → 原因）
}

func (e *Error) Error() string {
	return fmt.Sprintf("web: %d %s: %s", e.Status, e.Code, e.Message)
}

// WithDetails 补字段级细节（复制入参, 不持有调用方的 map）。
func (e *Error) WithDetails(details map[string]string) *Error {
	if len(details) == 0 {
		return e
	}
	clone := make(map[string]string, len(details))
	for name, message := range details {
		clone[name] = message
	}
	e.Details = clone
	return e
}

// Errorf 任意状态码 + 缺省码。
func Errorf(status int, format string, args ...any) *Error {
	return &Error{
		Status:  status,
		Code:    codeForStatus(status),
		Message: fmt.Sprintf(format, args...),
	}
}

func BadRequest(format string, args ...any) *Error {
	return Errorf(http.StatusBadRequest, format, args...)
}

// InvalidFields 字段级拒绝（422 + details）—— 白名单/校验都用它。
func InvalidFields(details map[string]string) *Error {
	err := Errorf(http.StatusUnprocessableEntity, "字段未通过校验")
	return err.WithDetails(details)
}

// Unauthorized 没有身份 —— 匿名被拒。
func Unauthorized(format string, args ...any) *Error {
	return Errorf(http.StatusUnauthorized, format, args...)
}

// Forbidden 有身份但没权限。
func Forbidden(format string, args ...any) *Error {
	return Errorf(http.StatusForbidden, format, args...)
}

func NotFound(format string, args ...any) *Error {
	return Errorf(http.StatusNotFound, format, args...)
}

// Conflict 状态冲突（乐观锁、唯一约束、被引用不许删）。
func Conflict(format string, args ...any) *Error {
	return Errorf(http.StatusConflict, format, args...)
}

// Internal 服务端自己的错。**消息不外泄**: 只有日志里看得到。
func Internal(format string, args ...any) *Error {
	return Errorf(http.StatusInternalServerError, format, args...)
}

// codeForStatus 状态码 → 缺省短码。没列的状态码给个通用的 —— 码是给人/脚本读的,
// 不为了它维护一张必须同步的枚举。
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusUnprocessableEntity:
		return "invalid"
	case http.StatusInternalServerError:
		return "internal"
	}
	return "error"
}

// CoreError 把内核的错误翻成 HTTP 语义。
//
// 内核的错误是**领域语言**（找不到 / 版本冲突 / 字段不合法 / 被引用不许删）,
// 翻译只在这一处做 —— 别在 handler 里散着写 errors.Is。
//
// 认不出的错误一律 500 且不把原消息给出去（内部信息不该变成响应体）。
func CoreError(err error) *Error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, core.ErrNotFound):
		return NotFound("不存在")
	case errors.Is(err, core.ErrRevisionConflict):
		return Conflict("数据已被他人修改, 请刷新后重试").WithCode("revision_conflict")
	case errors.Is(err, core.ErrInvalidFields):
		return Errorf(http.StatusUnprocessableEntity, "%s", trimCorePrefix(err))
	case errors.Is(err, core.ErrInvalidField),
		errors.Is(err, core.ErrInvalidOperator),
		errors.Is(err, core.ErrInvalidValue),
		errors.Is(err, core.ErrInvalidQuery),
		errors.Is(err, core.ErrQueryTooComplex):
		return Errorf(http.StatusBadRequest, "%s", trimCorePrefix(err))
	case errors.Is(err, core.ErrDeleteRestricted):
		return Conflict("%s", trimCorePrefix(err)).WithCode("referenced")
	case errors.Is(err, core.ErrRelationCardinality):
		return Conflict("%s", trimCorePrefix(err)).WithCode("cardinality")
	}
	return Internal("服务端错误")
}

// WithCode 覆盖短码（少数需要客户端分支的错误才用）。
func (e *Error) WithCode(code string) *Error {
	e.Code = code
	return e
}

// trimCorePrefix 去掉内核前缀（`core: `）—— 响应里不该带实现细节。
func trimCorePrefix(err error) string {
	message := err.Error()
	return strings.TrimPrefix(message, "core: ")
}
