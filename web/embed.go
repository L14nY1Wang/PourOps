package web

import "embed"

// TemplatesFS 内嵌全站所有 HTML 页面模板（后台、点餐端、电视大屏）。
//
//go:embed templates
var TemplatesFS embed.FS

// StaticFS 内嵌全站运行时静态资源（brand、css、js、screen 兜底数据与 vendor）。
//
//go:embed static
var StaticFS embed.FS
