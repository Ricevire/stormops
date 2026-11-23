// Package web 内嵌控制台静态资源。
//
// 用 go:embed 而不是把文件丢到磁盘上，是为了让 StormOps 保持"单二进制部署"：
// scp 一个文件上去就能跑，不需要额外同步前端资源，也不会出现
// "二进制更新了但前端还是旧版本"这种低级问题。
package web

import "embed"

// FS 内嵌了控制台的全部静态资源。
//
//go:embed index.html style.css app.js
var FS embed.FS
