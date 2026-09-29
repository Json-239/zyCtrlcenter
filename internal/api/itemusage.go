// 物品使用配置（只读）：面板「物品配置」页数据源。
//
//	GET /api/item_usage → {ok, available, path, version, title, count, category_count,
//	                       finding_count, unimplemented_count, catalog, msg?}
//
// 数据源 = <DataDir>/item_usage_catalog.json（机读版物品清单，配套文档
// docs/04-测试/清单-20260929-物品使用配置.md；由盘点脚本/人工维护）。
// 文件缺失或损坏 → available=false + msg（HTTP 仍是 200，前端优雅提示，不报错崩页）。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

// itemUsageFile data/ 下的机读数据文件名。
const itemUsageFile = "item_usage_catalog.json"

// handleItemUsage 物品使用配置（只读）。
func (a *API) handleItemUsage(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(a.Cfg.DataDir, itemUsageFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "available": false, "path": path, "count": 0,
			"msg": "物品配置数据未就绪：未找到 " + itemUsageFile + "（放到 data/ 后刷新即可）",
		})
		return
	}
	// 解析到通用结构只为取摘要计数；catalog 原样透传（避免数字被 float64 化、字段被裁剪）。
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "available": false, "path": path, "count": 0,
			"msg": "物品配置数据解析失败：" + err.Error(),
		})
		return
	}
	items, _ := doc["items"].([]any)
	cats, _ := doc["categories"].([]any)
	findings, _ := doc["findings"].([]any)
	unimpl, _ := doc["unimplemented"].([]any)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "available": true, "path": path,
		"version": doc["version"], "title": doc["title"],
		"count":               len(items),
		"category_count":      len(cats),
		"finding_count":       len(findings),
		"unimplemented_count": len(unimpl),
		"catalog":             json.RawMessage(raw),
	})
}
