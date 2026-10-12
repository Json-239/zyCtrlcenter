// gzip 响应压缩中间件（2026-10-12 性能止血：/api/status 默认瘦身后 ~0.2MB 未压缩，
// 压缩后 ~20KB 级，3s 轮询带宽/序列化开销大幅下降）。
//
// 范围：仅 GET + 路径前缀 /api/ + 客户端 Accept-Encoding 接受 gzip。
//   - /ws（WebSocket 升级）不走本中间件；
//   - POST/OPTIONS 等直通；
//   - 非 200 或响应已带 Content-Encoding（如上游已编码）直通不压；
//   - body < gzipMinBytes(1KB) 直通不压（压缩头开销可能大于收益；语义不变）。
//
// 压缩时补 Content-Encoding: gzip + Vary: Accept-Encoding（对反向代理/浏览器缓存正确）。
// 实现 = 先缓冲再决定：对 /api/ 的 JSON 响应（一次性写出）无副作用；
// /api/ 处理器不使用 Flusher/Hijacker（grep 核实），故不透传这两个接口。
package api

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
)

// gzipMinBytes 小于该字节数的响应不压缩（直通原样输出）。
const gzipMinBytes = 1024

// withGzip GET /api/* 透明 gzip 中间件。
func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet ||
			!strings.HasPrefix(r.URL.Path, "/api/") ||
			!gzipAccepted(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		next.ServeHTTP(gw, r)
		gw.finish()
	})
}

// gzipAccepted Accept-Encoding 是否接受 gzip（跳过显式 q=0 拒收；
// 不做完整 q 值排序 —— 只关心"能不能用 gzip"，有 gzip 且未拒就用）。
func gzipAccepted(ae string) bool {
	for _, part := range strings.Split(ae, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		name, params, _ := strings.Cut(p, ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		return !strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0")
	}
	return false
}

// gzipWriter 缓冲写出的 ResponseWriter 包装（只在 GET /api/* 分支使用）。
type gzipWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	buf         bytes.Buffer
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.status = code
	g.wroteHeader = true
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		g.status = http.StatusOK
		g.wroteHeader = true
	}
	return g.buf.Write(p)
}

// finish 提交缓冲：仅 200 + 无既有 Content-Encoding + body ≥ gzipMinBytes 才压，
// 其余原样直出（含状态码与 body，语义与未包装时一致）。
func (g *gzipWriter) finish() {
	status := g.status
	if status == 0 {
		status = http.StatusOK
	}
	h := g.ResponseWriter.Header()
	body := g.buf.Bytes()
	if status != http.StatusOK || h.Get("Content-Encoding") != "" || len(body) < gzipMinBytes {
		g.ResponseWriter.WriteHeader(status)
		if len(body) > 0 {
			_, _ = g.ResponseWriter.Write(body)
		}
		return
	}
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	g.ResponseWriter.WriteHeader(status)
	zw := gzip.NewWriter(g.ResponseWriter)
	_, _ = zw.Write(body)
	_ = zw.Close()
}
