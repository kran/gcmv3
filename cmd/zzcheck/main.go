package main

import (
	"fmt"
	"os"

	"github.com/kran/gcmv3/types"
)

func main() {
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	ts := types.New()
	err = ts.Load(raw)
	if err != nil {
		fmt.Println("加载失败:", err)
		os.Exit(1)
	}
	fmt.Printf("加载成功: %d 个类型\n", len(ts.Names()))
	for _, name := range ts.Names() {
		def, _ := ts.Type(name)
		fmt.Printf("  %-14s 字段 %2d 列 %v 地址=%v 登录=%v\n", name, len(def.Fields),
			def.Admin.Columns, ts.Addressable(name), def.Capabilities.Authentication != nil)
	}
}
