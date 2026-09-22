package maplib

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Grid 地图网格（阻挡位图）。rows[y][x]：'1'=阻挡，'0'/其它=可走。
type Grid struct {
	W    int      `json:"w"`
	H    int      `json:"h"`
	Rows []string `json:"rows"`
}

// Blocked 判断格子是否阻挡（越界视为阻挡）。
func (g *Grid) Blocked(gx, gy int) bool {
	if g == nil || gy < 0 || gy >= len(g.Rows) {
		return true
	}
	row := g.Rows[gy]
	if gx < 0 || gx >= len(row) {
		return true
	}
	return row[gx] == '1'
}

// GridReader 按需读取地图网格：游戏配置目录的 map.csv(block_file 列) → map_file/blockfile/<file>。
//
// blockfile 格式（参考实现 chainlib.parse_map_grids，已实测）：
//
//	字节 0..3  = 宽 w（uint32 小端）
//	字节 4..7  = 高 h（uint32 小端）
//	字节 8     = 分隔符（跳过）
//	字节 9..   = h 行、每行 w 个 '0'/'1' 字符（换行分隔）
//
// 每张图读一次后缓存；map.csv 变更（换服/换版本）会整体重载索引。
type GridReader struct {
	configDir string

	mu      sync.Mutex
	index   map[int]gridMeta
	loaded  bool
	mtime   time.Time
	size    int64
	cache   map[int]*Grid
	maxKeep int
}

type gridMeta struct {
	Name      string
	BlockFile string
}

// NewGridReader 创建读取器；configDir 为游戏配置目录（含 map.csv）。
func NewGridReader(configDir string) *GridReader {
	return &GridReader{configDir: configDir, index: map[int]gridMeta{}, cache: map[int]*Grid{}, maxKeep: 30}
}

// ConfigDir 游戏配置目录。
func (r *GridReader) ConfigDir() string { return r.configDir }

// Available 网格数据是否可用（map.csv 与 blockfile 目录存在）。
func (r *GridReader) Available() bool {
	if _, err := os.Stat(filepath.Join(r.configDir, "map.csv")); err != nil {
		return false
	}
	st, err := os.Stat(filepath.Join(r.configDir, "map_file", "blockfile"))
	return err == nil && st.IsDir()
}

// MapName 从 map.csv 取地图名（网格数据不存在时也能用）。
func (r *GridReader) MapName(mapid int) string {
	r.loadIndex()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.index[mapid].Name
}

// Grid 读取并缓存某图的网格。
func (r *GridReader) Grid(mapid int) (*Grid, error) {
	r.loadIndex()
	r.mu.Lock()
	meta, ok := r.index[mapid]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("map.csv 里没有 mapid=%d（或缺少 block_file 列）", mapid)
	}
	if g, ok := r.cache[mapid]; ok {
		r.mu.Unlock()
		return g, nil
	}
	r.mu.Unlock()

	path := filepath.Join(r.configDir, "map_file", "blockfile", meta.BlockFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取阻挡文件失败 %s: %w", path, err)
	}
	g, err := parseBlockFile(raw)
	if err != nil {
		return nil, fmt.Errorf("解析阻挡文件 %s: %w", path, err)
	}

	r.mu.Lock()
	if len(r.cache) >= r.maxKeep { // 简单容量控制：满了先清空（网格数据重读成本低）
		r.cache = map[int]*Grid{}
	}
	r.cache[mapid] = g
	r.mu.Unlock()
	return g, nil
}

// parseBlockFile 解析阻挡文件（格式见类型注释）。
func parseBlockFile(raw []byte) (*Grid, error) {
	if len(raw) < 9 {
		return nil, fmt.Errorf("文件过短（%d 字节）", len(raw))
	}
	w := int(binary.LittleEndian.Uint32(raw[0:4]))
	h := int(binary.LittleEndian.Uint32(raw[4:8]))
	if w <= 0 || h <= 0 || w > 10000 || h > 10000 {
		return nil, fmt.Errorf("尺寸异常: w=%d h=%d", w, h)
	}
	body := string(raw[9:])
	lines := strings.Split(body, "\n")
	rows := make([]string, 0, h)
	for i := 0; i < len(lines) && len(rows) < h; i++ {
		line := strings.TrimRight(lines[i], "\r")
		if line == "" {
			continue
		}
		if len(line) < w { // 短行按可走补齐（服务端文件偶有缺字符）
			line += strings.Repeat("0", w-len(line))
		}
		rows = append(rows, line[:w])
	}
	if len(rows) < h { // 行数不足：补空行（可走），保证前端能画
		for len(rows) < h {
			rows = append(rows, strings.Repeat("0", w))
		}
	}
	return &Grid{W: w, H: h, Rows: rows}, nil
}

// loadIndex 解析 map.csv（map_index, map_name, map_type, block_file …），文件变了才重载。
func (r *GridReader) loadIndex() {
	path := filepath.Join(r.configDir, "map.csv")
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	r.mu.Lock()
	if r.loaded && st.ModTime().Equal(r.mtime) && st.Size() == r.size {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	index := map[int]gridMeta{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 4 {
			continue
		}
		id, err := parseInt(strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		index[id] = gridMeta{
			Name:      strings.TrimSpace(parts[1]),
			BlockFile: strings.TrimSpace(parts[3]),
		}
	}
	r.mu.Lock()
	r.index, r.loaded = index, true
	r.mtime, r.size = st.ModTime(), st.Size()
	if len(r.cache) > 0 {
		r.cache = map[int]*Grid{} // 索引变了，旧网格作废
	}
	r.mu.Unlock()
}

func parseInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(ch-'0')
	}
	return n, nil
}
