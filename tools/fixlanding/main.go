// fixlanding 用真实 blockfile 校验并修正"抓鬼落点"（ghost_map_pos）。
//
// 背景：落点原先按"每张刷鬼图挑第一个 NPC 坐标"选，客观核查发现图9 的 (481,604) 落在阻挡格
// （筋斗云落地会卡）。本工具改成：**同图 NPC 坐标里挑第一个可走格**；都不行就 BFS 找最近可走格。
//
// 只重写目标文件的 ghost_map_pos，其余字段原样保留（中控对链数据只搬运，工具也不改别的）。
//
// 用法（默认 dry-run，只报告不写）：
//
//	go run ./tools/fixlanding                       # 报告
//	go run ./tools/fixlanding -write                # 写回 zhuaogui_nav.json
//	go run ./tools/fixlanding -file xxx.json -game <游戏服 config 目录>
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"zyctrlcenter/internal/maplib"
)

const (
	defaultGame = `F:\ZyBin\xm\2d-xiyou-server\config`
	gridCell    = 16
	maxRadius   = 16 // 最近可走格搜索半径（格）
)

func main() {
	file := flag.String("file", "zhuaogui_nav.json", "链数据文件名（放链目录里）")
	chains := flag.String("chains", "", "链数据目录（默认 <仓库>/data/chains）")
	game := flag.String("game", defaultGame, "游戏服配置目录（含 map_file/blockfile）")
	write := flag.Bool("write", false, "写回文件（默认只报告）")
	flag.Parse()

	dir := *chains
	if dir == "" {
		exe, _ := os.Executable()
		_ = exe
		wd, _ := os.Getwd()
		dir = filepath.Join(wd, "data", "chains")
	}
	path := filepath.Join(dir, *file)
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("读文件失败:", err)
		os.Exit(1)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		fmt.Println("解析失败:", err)
		os.Exit(1)
	}
	var ghostMaps []int
	if v, ok := top["ghost_maps"]; ok {
		_ = json.Unmarshal(v, &ghostMaps)
	}
	var ghostPos map[string][]int
	if v, ok := top["ghost_map_pos"]; ok {
		_ = json.Unmarshal(v, &ghostPos)
	}
	npcs := parseNPCs(top["npcs"])
	gr := maplib.NewGridReader(*game)
	if !gr.Available() {
		fmt.Println("没有游戏服配置目录:", *game)
		os.Exit(1)
	}

	fixed := map[string][]int{}
	changed := 0
	for _, m := range ghostMaps {
		old := ghostPos[strconv.Itoa(m)]
		np, err := gr.Grid(m)
		if err != nil || np == nil {
			fmt.Printf("图 %d : 没有网格，保持原样 %v\n", m, old)
			if len(old) == 2 {
				fixed[strconv.Itoa(m)] = old
			}
			continue
		}
		// 1) 同图 NPC 坐标里挑第一个可走的
		best, src := [2]int{}, ""
		for _, p := range candidatesInMap(npcs, m) {
			g := maplib.GridPosWith(p[0], p[1], gridCell)
			if !np.Blocked(g[0], g[1]) {
				best, src = p, "同图NPC坐标"
				break
			}
		}
		// 2) 都不行：从原落点（或第一个候选）出发 BFS 找最近可走格
		if src == "" {
			from := [2]int{0, 0}
			if len(old) == 2 {
				from = [2]int{old[0], old[1]}
			} else if cs := candidatesInMap(npcs, m); len(cs) > 0 {
				from = cs[0]
			}
			if p, ok := nearestWalkable(np, from[0], from[1]); ok {
				best, src = p, "最近可走格"
			}
		}
		if src == "" {
			fmt.Printf("图 %d : 找不到可走落点，保持原样 %v\n", m, old)
			if len(old) == 2 {
				fixed[strconv.Itoa(m)] = old
			}
			continue
		}
		fixed[strconv.Itoa(m)] = []int{best[0], best[1]}
		blocked := len(old) == 2 && np.Blocked(maplib.GridPosWith(old[0], old[1], gridCell)[0],
			maplib.GridPosWith(old[0], old[1], gridCell)[1])
		if blocked {
			changed++
			fmt.Printf("图 %d : %v（阻挡）→ %v（%s）\n", m, old, []int{best[0], best[1]}, src)
		} else {
			fmt.Printf("图 %d : %v 本来就可走，保持\n", m, old)
		}
	}

	if !*write {
		fmt.Printf("\n[dry-run] 需修正 %d 个落点；加 -write 写回\n", changed)
		return
	}
	blob, err := json.Marshal(fixed)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	top["ghost_map_pos"] = blob
	out, err := json.Marshal(top)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		fmt.Println("写回失败:", err)
		os.Exit(1)
	}
	fmt.Printf("\n已写回 %s（修正 %d 个落点）\n", path, changed)
}

// candidatesInMap npcs 里所有落在图 m 的坐标（按 npc 号数值排序，保证确定性）。
func candidatesInMap(npcs map[string][][3]int, m int) [][2]int {
	keys := make([]string, 0, len(npcs))
	for k := range npcs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, _ := strconv.Atoi(keys[i])
		b, _ := strconv.Atoi(keys[j])
		return a < b
	})
	out := [][2]int{}
	for _, k := range keys {
		for _, p := range npcs[k] {
			if p[0] == m {
				out = append(out, [2]int{p[1], p[2]})
			}
		}
	}
	return out
}

func nearestWalkable(g *maplib.Grid, x, y int) ([2]int, bool) {
	c := maplib.GridPosWith(x, y, gridCell)
	for r := 0; r <= maxRadius; r++ {
		for dy := -r; dy <= r; dy++ {
			for dx := -r; dx <= r; dx++ {
				if maxInt(abs(dx), abs(dy)) != r {
					continue // 只看这一圈
				}
				gx, gy := c[0]+dx, c[1]+dy
				if gx < 0 || gy < 0 || g.Blocked(gx, gy) {
					continue
				}
				return [2]int{gx*gridCell + gridCell/2, gy*gridCell + gridCell/2}, true
			}
		}
	}
	return [2]int{}, false
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// parseNPCs 兼容扁平 [map,x,y] 与嵌套 [[map,x,y],…] 两种形状。
func parseNPCs(raw json.RawMessage) map[string][][3]int {
	out := map[string][][3]int{}
	if len(raw) == 0 {
		return out
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	for k, v := range m {
		var flat []int
		if json.Unmarshal(v, &flat) == nil && len(flat) == 3 {
			out[k] = [][3]int{{flat[0], flat[1], flat[2]}}
			continue
		}
		var nested [][]int
		if json.Unmarshal(v, &nested) == nil {
			ps := [][3]int{}
			for _, p := range nested {
				if len(p) == 3 {
					ps = append(ps, [3]int{p[0], p[1], p[2]})
				}
			}
			if len(ps) > 0 {
				out[k] = ps
			}
		}
	}
	return out
}

var _ = strings.TrimSpace
