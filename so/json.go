package so

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ParseJSON JSON 数组前端。与 Lisp 是**同一棵树**, 所以这里只有解码 ——
// 算符表、复杂度上限、读树逻辑全都与 Lisp 共用。
//
//	["and", ["=","$state","published"], ["in","->categories",[1,2,3]]]
//
// 没有占位符: JSON 直接在值的位置写值（模板参数由调用方在 Go 侧拼好）。
//
// 解析器是标准库 —— 不用写 lexer, 不用管转义, 也不用管括号配对;
// 代价是与 Lisp 相比手写更啰嗦（这是给后台过滤 UI / SDK **生成**用的形态,
// 不是给人手打的）。
func ParseJSON(src string) (Expr, error) {
	if len(src) > MaxFilterBytes {
		return nil, fmt.Errorf("query: json expression exceeds %d bytes", MaxFilterBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(src))
	decoder.UseNumber() // 保 int64 精度; 归一成 int64/float64 在 Registry.Read 里
	var tree any
	err := decoder.Decode(&tree)
	if err != nil {
		return nil, fmt.Errorf("query: json: %w", err)
	}
	var trailing any
	err = decoder.Decode(&trailing)
	if !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("query: json must contain exactly one value")
		}
		return nil, fmt.Errorf("query: json: %w", err)
	}
	expr, err := NewReader().Read(tree)
	if err != nil {
		return nil, fmt.Errorf("query: json: %w", err)
	}
	return expr, nil
}
