// 此程序只供隔离机模拟候选启动破坏数据库后退出，不进入发行资产。
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("v0.5.0")
		return
	}
	if err := os.WriteFile("/var/lib/heron/heron.db", []byte("broken candidate database"), 0600); err != nil {
		panic(err)
	}
	os.Exit(1)
}
