// main.go workbuddy2api 独立入口：解析参数后交给 internal/gwapi 启动网关。
//
// 网关的全部启动逻辑在 internal/gwapi.Run（原先是这个文件里的 main 函数体）。
// 抽出去是为了让统一程序 cmd/wbone 复用同一套装配代码——桌面管家和纯服务端
// 走同一条路径，行为不会分叉。
package main

import (
	"context"
	"flag"
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gwapi"
)

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	flag.Parse()

	if err := gwapi.Run(context.Background(), *cfgPath, nil); err != nil {
		log.Fatalf("%v", err)
	}
}
