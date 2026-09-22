package api

// 静态资源托管：前端面板（web/dist）存在时由中控直接服务，git clone 后无需再开 vite dev server。
//
// 规则（2026-09-22）：
//   - GET /           → 有 dist（index.html）→ 面板首页；没有 → JSON 入口提示（原行为 + 一键脚本指引）
//   - GET /<其余路径>  → 有 dist → 命中文件则原样返回，未命中回落 index.html（SPA 前端路由）
//   - /api、/ws 前缀   → 不接管（API 未知路径仍是 404 JSON，不会被吞成 HTML）
//
// 面板构建产物不入库（见 .gitignore 的 web/dist/），目录不存在即完全保持旧行为。

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// webDistIndex 返回可用的面板入口文件（<WebDistDir>/index.html）；未构建时返回 ""。
func (a *API) webDistIndex() string {
	dir := strings.TrimSpace(a.Cfg.WebDistDir)
	if dir == "" {
		return ""
	}
	index := filepath.Join(dir, "index.html")
	st, err := os.Stat(index)
	if err != nil || st.IsDir() {
		return ""
	}
	return index
}

// handleRoot "/"：面板已构建 → 首页；否则 JSON 入口提示。
func (a *API) handleRoot(w http.ResponseWriter, r *http.Request) {
	if a.serveWebDist(w, r) {
		return
	}
	a.handleIndex(w, r)
}

// handleWebFallback 其余路径（含未注册的 /api/xxx）：面板已构建 → 静态文件 / SPA 回落；
// 否则维持 404 JSON。只接管 GET/HEAD，写接口打到静态路径时不改变 404 语义。
func (a *API) handleWebFallback(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/api" || strings.HasPrefix(p, "/api/") || p == "/ws" || strings.HasPrefix(p, "/ws/") {
		a.handleNotFound(w, r)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if a.serveWebDist(w, r) {
			return
		}
	}
	a.handleNotFound(w, r)
}

// serveWebDist 用 WebDistDir 响应请求；返回 false = 面板未构建（调用方按原逻辑处理）。
//
// 安全：路径经 path.Clean 归一化后只取相对部分拼盘内路径（".." 无法越出 dist）。
func (a *API) serveWebDist(w http.ResponseWriter, r *http.Request) bool {
	index := a.webDistIndex()
	if index == "" {
		return false
	}
	dir := filepath.Dir(index)

	rel := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	target := index // 未命中文件时回落入口（SPA）
	if rel != "" {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if st, err := os.Stat(full); err == nil && !st.IsDir() {
			target = full
		}
	}

	switch {
	case target == index:
		// 入口（含 SPA 回落）：不缓存，重新构建后刷新即生效。
		w.Header().Set("Cache-Control", "no-cache")
	case strings.HasPrefix(rel, "assets/"):
		// vite 产物文件名带内容哈希：可长缓存。
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeFile(w, r, target) // ServeFile 按扩展名给 Content-Type（回落时是 text/html）
	return true
}
