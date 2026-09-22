package chainlib

import "encoding/json"

// 本文件提供「未知字段原样透传」的通用实现。
//
// 为什么需要：链数据是机器人端消费的事实数据（真实导出含 catcher_name/thrower_npc/
// ghost_map_pos/item_meta 等大量字段），中控只做搬运 —— 若用强类型结构体直接丢弃
// 未声明字段，下发给机器人的链数据就会缺字段，属于静默损坏。
//
// 约定：结构体声明 `Extra map[string]json.RawMessage \`json:"-"\``，
// 在 UnmarshalJSON 里 splitExtra、在 MarshalJSON 里 mergeExtra。

// splitExtra 从原始 JSON 中剔除 known 键，其余原样返回（用于保留未知字段）。
func splitExtra(data []byte, known []string) (map[string]json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	for _, k := range known {
		delete(all, k)
	}
	return all, nil
}

// mergeExtra 把 extra 合并进 base JSON（已存在的键以 base 为准），键按字典序输出。
func mergeExtra(base []byte, extra map[string]json.RawMessage) ([]byte, error) {
	if len(extra) == 0 {
		return base, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for k, v := range extra {
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}
	return json.Marshal(merged)
}
