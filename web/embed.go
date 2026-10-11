// Package web 只做一件事: 把 web/static 下的原生多页前端打进二进制。
//
// 单独建一个包是因为 go:embed 无法跨目录引用 —— 面板路由在 internal/panel,
// 而前端源码放在仓库根的 web/static (方便直接改 HTML 后重新编译, 不必进 internal)。
//
// 不用 all: 前缀的变体: static 下没有需要保留的下划线/点开头文件。
package web

import "embed"

// Static 是 web/static 的只读快照。server.go 用 fs.Sub(Static, "static") 取根。
//
//go:embed static
var Static embed.FS
