package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kran/gcmv3/core"
)

// core 的错误 → HTTP 语义的翻译只有这一处。
func TestCoreErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"找不到", core.ErrNotFound, http.StatusNotFound, "not_found"},
		{"版本冲突", core.ErrRevisionConflict, http.StatusConflict, "revision_conflict"},
		{"字段不合法", fmt.Errorf("%w: name: 必填", core.ErrInvalidFields),
			http.StatusUnprocessableEntity, "invalid"},
		{"字段不存在", core.ErrInvalidField, http.StatusBadRequest, "bad_request"},
		{"算符不存在", core.ErrInvalidOperator, http.StatusBadRequest, "bad_request"},
		{"值不合法", core.ErrInvalidValue, http.StatusBadRequest, "bad_request"},
		{"查询不合法", core.ErrInvalidQuery, http.StatusBadRequest, "bad_request"},
		{"查询太复杂", core.ErrQueryTooComplex, http.StatusBadRequest, "bad_request"},
		{"被引用不许删", core.ErrDeleteRestricted, http.StatusConflict, "referenced"},
		{"引用基数", core.ErrRelationCardinality, http.StatusConflict, "cardinality"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := CoreError(test.err)
			if got == nil || got.Status != test.status || got.Code != test.code {
				t.Fatalf("CoreError = %#v, want %d/%s", got, test.status, test.code)
			}
		})
	}
	if CoreError(nil) != nil {
		t.Fatal("nil 该原样返回 nil")
	}
}

// 认不出的错误 ⇒ 500 且**不泄漏原消息**（内部信息不该变成响应体）。
func TestCoreErrorHidesUnknown(t *testing.T) {
	secret := errors.New("数据库密码是 hunter2")
	got := CoreError(secret)
	if got.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d", got.Status)
	}
	if strings.Contains(got.Message, "hunter2") {
		t.Fatalf("消息泄漏了内部信息: %q", got.Message)
	}
}

// 带前缀的 core 消息在响应里去掉 `core: `。
func TestCoreErrorTrimsPrefix(t *testing.T) {
	got := CoreError(fmt.Errorf("%w: title: 太长", core.ErrInvalidFields))
	if strings.Contains(got.Message, "core:") {
		t.Fatalf("消息不该带实现细节: %q", got.Message)
	}
	if !strings.Contains(got.Message, "title: 太长") {
		t.Fatalf("有用的部分该留下: %q", got.Message)
	}
}

// 缺省短码由状态码推出; WithCode 可覆盖。
func TestErrorCodes(t *testing.T) {
	cases := map[*Error]string{
		BadRequest("x"):                "bad_request",
		Unauthorized("x"):              "unauthorized",
		Forbidden("x"):                 "forbidden",
		NotFound("x"):                  "not_found",
		Conflict("x"):                  "conflict",
		InvalidFields(nil):             "invalid",
		Internal("x"):                  "internal",
		Errorf(http.StatusTeapot, "x"): "error",
	}
	for err, want := range cases {
		if err.Code != want || err.Status == 0 || err.Message == "" {
			t.Fatalf("%#v: code = %q, want %q", err, err.Code, want)
		}
	}
	if got := Conflict("x").WithCode("custom").Code; got != "custom" {
		t.Fatalf("code = %q", got)
	}
}

// details 是拷贝: 调用方之后改自己的 map 不影响错误。
func TestInvalidFieldsCopiesDetails(t *testing.T) {
	details := map[string]string{"title": "太长"}
	err := InvalidFields(details)
	details["title"] = "改了"
	if err.Details["title"] != "太长" {
		t.Fatalf("details = %#v", err.Details)
	}
	if InvalidFields(nil).Details != nil {
		t.Fatal("空 details 该保持 nil")
	}
}

// 错误串带上状态码与短码（日志里能读）。
func TestErrorString(t *testing.T) {
	got := Forbidden("仅作者可改").Error()
	for _, want := range []string{"403", "forbidden", "仅作者可改"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q 该含 %q", got, want)
		}
	}
}
