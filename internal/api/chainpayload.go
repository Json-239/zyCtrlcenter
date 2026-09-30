// 链数据载荷提供者：把"要随命令下发的链数据"集中到一处（按文件缓存，改了自动重载）。
//
// 为什么单独抽出来：**抓鬼（ghost_start）必须带导航数据下发**，否则机器人拿不到
// npcs/map_grids/dijkstra/ghost_maps/ghost_map_pos，表现是"原地不动"；而恢复引擎
// （internal/services/restorer）在 main.go 里比 API 实例先构造，需要一个不依赖 API
// 实例的载荷入口 —— 两边共用同一个 Payloads，缓存也只有一份。
//
// 抓鬼载荷的组装口径（对齐参考实现 `services/chain.py: ghost_nav_payload()`）：
//
//	基座链（默认 newbie_full，配置 GhostBaseChainID）
//	  → npcs（钟馗等所有 NPC 坐标，全量） / map_grids（寻路网格） / dijkstra（跨图路由）
//	    / grid_cell / item_meta
//	+ 抓鬼专属文件（GhostNavChainID，默认 zhongkui_nav，**只需放这三项**）
//	  → ghost_maps（刷鬼图列表，参考实现从 20195.xml 的"捉鬼npc"解析）
//	  / ghost_map_pos（每个刷鬼图的筋斗云落点，参考实现由 npcs 里该图第一个 NPC 推导）
//	  / maps（地图名，可选）
//
// 也就是说"地址"来自链数据本身（npcs 里的坐标 + 网格 + 路由），抓鬼专属字段只是"去哪张图、
// 落在哪"；这样链数据一更新，抓鬼导航自动跟随，不会出现"新手链换了、抓鬼还用老网格"。
//
// 口径：
//   - 载荷原样透传（复用 chainlib，未声明字段与形状都不改）；合并只发生在 map 类字段（按 key，专属优先）；
//   - **地址完备性硬校验**：刷鬼图必须有网格、必须有落点、npcs/dijkstra 非空——不齐就报错（不发空命令）；
//   - 返回 *chainlib.Chain 而不是 map[string]any：避免 2MB 数据在 map 往返里丢大整数精度。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"zyctrlcenter/internal/chainlib"
	"zyctrlcenter/internal/config"
)

// Payloads 链数据载荷提供者（按文件缓存：文件被替换/重新导出后自动重载）。
type Payloads struct {
	cfg *config.Config

	mu    sync.Mutex
	cache map[string]cachedChain
	ghost map[string]cachedGhost // 组装结果（键 = 基座版本 + 专属版本）
	daily map[string]cachedGhost // 分享日常组装结果（同口径）
}

type cachedChain struct {
	version string // "<size>:<mtimeUnixNano>"，文件一变就换版本
	chain   *chainlib.Chain
}

type cachedGhost struct {
	chain *chainlib.Chain
}

// NewPayloads 创建载荷提供者。
func NewPayloads(cfg *config.Config) *Payloads {
	return &Payloads{cfg: cfg, cache: map[string]cachedChain{}, ghost: map[string]cachedGhost{}, daily: map[string]cachedGhost{}}
}

// GhostNavChainID 抓鬼专属字段所在文件（配置项 CTRL_GHOST_NAV_CHAIN，默认 zhongkui_nav）。
func (p *Payloads) GhostNavChainID() string {
	if p == nil || p.cfg == nil {
		return "zhongkui_nav"
	}
	if id := strings.TrimSpace(p.cfg.GhostNavChainID); id != "" {
		return id
	}
	return "zhongkui_nav"
}

// GhostBaseChainID 抓鬼导航的基座链（配置项 CTRL_GHOST_BASE_CHAIN，默认 newbie_full）。
func (p *Payloads) GhostBaseChainID() string {
	if p == nil || p.cfg == nil {
		return "newbie_full"
	}
	if id := strings.TrimSpace(p.cfg.GhostBaseChainID); id != "" {
		return id
	}
	return "newbie_full"
}

// GhostDailyLimit 抓鬼每日上限（随命令下发，与机器人默认 50 同口径）。
func (p *Payloads) GhostDailyLimit() int {
	if p == nil || p.cfg == nil || p.cfg.GhostDailyLimit <= 0 {
		return 50
	}
	return p.cfg.GhostDailyLimit
}

// ShareDailyChainID 分享日常专属声明文件（配置项 CTRL_SHARE_DAILY_CHAIN，默认 shenbu_nav）。
func (p *Payloads) ShareDailyChainID() string {
	if p == nil || p.cfg == nil || strings.TrimSpace(p.cfg.ShareDailyChainID) == "" {
		return "shenbu_nav"
	}
	return strings.TrimSpace(p.cfg.ShareDailyChainID)
}

// ShareDailyKey 分享日常玩法键（配置项 CTRL_SHARE_DAILY_KEY，默认 share_daily_大唐神捕）。
func (p *Payloads) ShareDailyKey() string {
	if p == nil || p.cfg == nil || strings.TrimSpace(p.cfg.ShareDailyKey) == "" {
		return "share_daily_大唐神捕"
	}
	return strings.TrimSpace(p.cfg.ShareDailyKey)
}

// ShareDailyLimit 分享日常日限（配置项 CTRL_SHARE_DAILY_LIMIT，默认 10）。
func (p *Payloads) ShareDailyLimit() int {
	if p == nil || p.cfg == nil || p.cfg.ShareDailyDailyLimit <= 0 {
		return 10
	}
	return p.cfg.ShareDailyDailyLimit
}

// FenghuoChainID 烽火大唐专属声明文件（配置项 CTRL_FENGHUO_CHAIN，默认 fenghuo_nav）。
func (p *Payloads) FenghuoChainID() string {
	if p == nil || p.cfg == nil || strings.TrimSpace(p.cfg.FenghuoChainID) == "" {
		return "fenghuo_nav"
	}
	return strings.TrimSpace(p.cfg.FenghuoChainID)
}

// FenghuoKey 烽火大唐玩法键（配置项 CTRL_FENGHUO_KEY，默认 share_daily_宫廷10）。
func (p *Payloads) FenghuoKey() string {
	if p == nil || p.cfg == nil || strings.TrimSpace(p.cfg.FenghuoKey) == "" {
		return "share_daily_宫廷10"
	}
	return strings.TrimSpace(p.cfg.FenghuoKey)
}

// FenghuoDailyLimit 烽火大唐日限（配置项 CTRL_FENGHUO_LIMIT，默认 20，服务端 20021.xml 口径）。
func (p *Payloads) FenghuoDailyLimit() int {
	if p == nil || p.cfg == nil || p.cfg.FenghuoDailyLimit <= 0 {
		return 20
	}
	return p.cfg.FenghuoDailyLimit
}

// BiaoxingChainID 镖行天下专属声明文件（配置项 CTRL_BIAOXING_CHAIN，默认 biaoxing_nav）。
func (p *Payloads) BiaoxingChainID() string {
	if p == nil || p.cfg == nil || strings.TrimSpace(p.cfg.BiaoxingChainID) == "" {
		return "biaoxing_nav"
	}
	return strings.TrimSpace(p.cfg.BiaoxingChainID)
}

// BiaoxingKey 镖行天下玩法键（配置项 CTRL_BIAOXING_KEY，默认 share_daily_镖行天下）。
func (p *Payloads) BiaoxingKey() string {
	if p == nil || p.cfg == nil || strings.TrimSpace(p.cfg.BiaoxingKey) == "" {
		return "share_daily_镖行天下"
	}
	return strings.TrimSpace(p.cfg.BiaoxingKey)
}

// BiaoxingDailyLimit 镖行天下日限（配置项 CTRL_BIAOXING_LIMIT，默认 40，服务端 20011.xml 口径）。
func (p *Payloads) BiaoxingDailyLimit() int {
	if p == nil || p.cfg == nil || p.cfg.BiaoxingDailyLimit <= 0 {
		return 40
	}
	return p.cfg.BiaoxingDailyLimit
}

// ShareDailyParams 分享日常家族按 kind 取补发参数：返回该玩法的 (share_key, daily_limit)。
//
// kind = 意图 kind 字符串（shenbu / fenghuo，大小写不敏感）；认不出的（含空）回落神捕 ——
// 与 P0 单链口径一致（当时 share_daily_start 补发一律神捕）。
func (p *Payloads) ShareDailyParams(kind string) (string, int) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "fenghuo":
		return p.FenghuoKey(), p.FenghuoDailyLimit()
	case "biaoxing":
		return p.BiaoxingKey(), p.BiaoxingDailyLimit()
	default:
		return p.ShareDailyKey(), p.ShareDailyLimit()
	}
}

// ShareDaily 分享日常（大唐神捕）的链载荷 —— ShareDailyOf 的固定声明文件入口
// （配置项 CTRL_SHARE_DAILY_CHAIN，默认 shenbu_nav）。
func (p *Payloads) ShareDaily() (*chainlib.Chain, error) {
	return p.ShareDailyOf(p.ShareDailyChainID())
}

// ShareDailyOf 分享日常家族（shenbu / fenghuo）的链载荷：基座链（坐标/网格/路由）+
// 指定专属声明文件（task_order 等）。
//
// 与抓鬼导航同口径（方案 §4.2/§7）：机器人端只认 cmd["chain"]；专属文件只放**玩法声明**
// （task_order 必须列全分支任务号 —— 少列会让后续环节被机器人当"链外任务"静默忽略，R1），
// npcs/map_grids/dijkstra 从 GhostBaseChainID（默认 newbie_full）自动复用。
//
// navID = 声明文件名（shenbu_nav / fenghuo_nav / 配置覆盖值）。两个玩法共用本函数：
// 组装结果按 "navID|基座版本|声明版本" 缓存（不同玩法不互相顶掉）。
//
// 硬校验（宁可明确报错，也不发一份"跑不动/少环节"的载荷）：task_order 非空且每条能解析出
// task_index、基座 npcs/dijkstra 非空。文件缺失/解析失败同样硬失败（调用方一条命令都不发）。
func (p *Payloads) ShareDailyOf(navID string) (*chainlib.Chain, error) {
	navID = strings.TrimSpace(navID)
	if navID == "" {
		return nil, errors.New("分享日常声明文件名不能为空")
	}
	baseID := p.GhostBaseChainID()
	base, baseVer, err := p.byIDVersioned(baseID)
	if err != nil {
		return nil, fmt.Errorf("分享日常的基座链不可用（%s）：%w", baseID, err)
	}
	nav, navVer, err := p.byIDVersioned(navID)
	if err != nil {
		return nil, fmt.Errorf("分享日常声明文件不可用（%s）：%w", navID, err)
	}
	key := navID + "|" + baseVer + "|" + navVer
	p.mu.Lock()
	if c, ok := p.daily[key]; ok {
		p.mu.Unlock()
		return c.chain, nil
	}
	p.mu.Unlock()

	out := assembleShareDaily(base, nav)
	if err := validateShareDaily(out, baseID, navID); err != nil {
		return nil, err
	}
	if out.ChainID == "" {
		out.ChainID = navID
	}
	p.mu.Lock()
	// 组装结果随两文件版本变化：条目本身不失效，但每份 2MB 级 —— 超过 4 条（两个玩法 ×
	// 版本切换）时整表重来，只留当前这份，避免文件反复替换时长跑进程内存无界增长。
	if len(p.daily) > 4 {
		p.daily = make(map[string]cachedGhost, 2)
	}
	p.daily[key] = cachedGhost{chain: out}
	p.mu.Unlock()
	return out, nil
}

// Ghost 抓鬼导航数据：基座链（坐标/网格/路由）+ 抓鬼专属（刷鬼图/落点/地图名）。
// 文件缺失、解析失败、地址不全都返回 error —— 调用方必须**硬失败**（不发空命令）。
func (p *Payloads) Ghost() (*chainlib.Chain, error) {
	baseID, navID := p.GhostBaseChainID(), p.GhostNavChainID()
	base, baseVer, err := p.byIDVersioned(baseID)
	if err != nil {
		return nil, fmt.Errorf("抓鬼导航的基座链不可用（%s）：%w", baseID, err)
	}
	nav, navVer, err := p.byIDVersioned(navID)
	if err != nil {
		return nil, err
	}
	key := baseVer + "|" + navVer
	p.mu.Lock()
	if c, ok := p.ghost[key]; ok {
		p.mu.Unlock()
		return c.chain, nil
	}
	p.mu.Unlock()

	out := assembleGhostNav(base, nav)
	if err := validateGhostNav(out, baseID, navID); err != nil {
		return nil, err
	}
	if out.ChainID == "" {
		out.ChainID = "ghost_daily"
	}
	p.mu.Lock()
	p.ghost = map[string]cachedGhost{key: {chain: out}} // 只留最新一份（组装结果随两文件版本变化）
	p.mu.Unlock()
	return out, nil
}

// For 按意图 kind 取"要随命令下发的载荷"（restorer.Deps.Payload 用）。
//
//   - ghost → 抓鬼导航数据（不给就是原地不动，所以必须给）；
//   - shenbu → 大唐神捕链载荷（基座 + shenbu_nav 声明组装；不给机器人拿不到 task_order 与导航）；
//   - fenghuo → 烽火大唐链载荷（基座 + fenghuo_nav 声明组装；同上）；
//   - 其它 kind → nil：新手链/捉鬼链的补发只带 chain_id，机器人端有链缓存与网格缓存
//     （quest_engine 的 g_chain_cache / g_chain_grid_cache），避免每条补发都塞 2MB。
func (p *Payloads) For(kind, chainID string) (any, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "ghost":
		return p.Ghost()
	case "shenbu":
		return p.ShareDaily()
	case "fenghuo":
		return p.ShareDailyOf(p.FenghuoChainID())
	}
	return nil, nil
}

// Walk 游荡（random_walk / hatch_start）的导航载荷：目标图必须有寻路网格、跨图路由必须非空。
//
// 为什么单独一个入口：游荡要做两件事——"从当前图跨图走到目标图"（dijkstra 路由）+"在目标图里随机走"
// （map_grids 网格，机器人端 A*）。机器人端只认命令里的 chain，所以必须由中控组装下发。
//
// 复用**基座链**（GhostBaseChainID，默认 newbie_full：npcs/map_grids/dijkstra/grid_cell/item_meta）
// 原样下发，不做裁剪——跨图会经过中间图，中间图的网格同样要带（只留目标图 = 走到一半卡住）。
//
// 硬校验（与抓鬼载荷同口径：宁可明确报错，也不发一份"走不过去"的载荷）：
//   - mapid > 0；
//   - 目标图在 map_grids 里（否则到图后只能直线走 = 穿墙/到不了怪区）；
//   - dijkstra 非空（否则从当前图根本过不去）。
func (p *Payloads) Walk(mapid int) (*chainlib.Chain, error) {
	base, err := p.walkBase()
	if err != nil {
		return nil, err
	}
	if mapid <= 0 {
		return nil, fmt.Errorf("目标图 mapid 不合法：%d", mapid)
	}
	if _, ok := base.MapGrids[strconv.Itoa(mapid)]; !ok {
		return nil, fmt.Errorf("链数据 %s 的 map_grids 里没有图 %d 的寻路网格（现有 %d 张图：%s）："+
			"换一张有网格的图，或先把该图的网格补进链数据",
			p.GhostBaseChainID(), mapid, len(base.MapGrids), gridKeysText(base))
	}
	if len(base.Dijkstra) == 0 {
		return nil, fmt.Errorf("链数据 %s 缺 dijkstra（跨图路由）：游荡号从当前图走不到目标图", p.GhostBaseChainID())
	}
	// 浅拷贝：调用方（handler）不该拿到缓存里的同一份指针（沿用基座链的原始字段，不做裁剪）
	return cloneWalkChain(base), nil
}

// WalkRandom 随机图（mapid="random"）的导航载荷：**目标图由机器人端挑**（每号不同，见
// random_walk.py:resolve_roam_target），所以这里不做"具体某图"的校验，只保证"能挑出图"：
//
//   - maps 白名单给了：白名单里至少有一张图在 map_grids 里（否则机器人端挑不出；
//     这属于"配置/口径错"，宁可在这里明确报错，也不要发下去让每个号各自失败）；
//   - maps 没给：随机池 = 全部有网格的图，map_grids 非空即可；
//   - dijkstra 非空（跨图路由；随机图同样要跨图）。
//
// 注意：**这里不替机器人端抽签**——每号独立随机（避免全号扎堆同一张图）由机器人端做。
func (p *Payloads) WalkRandom(maps []int) (*chainlib.Chain, error) {
	base, err := p.walkBase()
	if err != nil {
		return nil, err
	}
	if len(base.Dijkstra) == 0 {
		return nil, fmt.Errorf("链数据 %s 缺 dijkstra（跨图路由）：游荡号从当前图走不到目标图", p.GhostBaseChainID())
	}
	if len(maps) == 0 {
		if len(base.MapGrids) == 0 {
			return nil, fmt.Errorf("链数据 %s 的 map_grids 为空：随机图没有可挑的目标图", p.GhostBaseChainID())
		}
		return cloneWalkChain(base), nil
	}
	usable := make([]string, 0, len(maps))
	for _, m := range maps {
		if _, ok := base.MapGrids[strconv.Itoa(m)]; ok {
			usable = append(usable, strconv.Itoa(m))
		}
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf("maps 白名单 %v 里没有任何一张图有寻路网格（链数据 %s 现有网格图：%s）："+
			"机器人端挑不出目标图，要么改白名单、要么先把这些图的网格补进链数据",
			maps, p.GhostBaseChainID(), gridKeysText(base))
	}
	return cloneWalkChain(base), nil
}

// walkBase 取"游荡导航"的基座链（GhostBaseChainID，默认 newbie_full）。
// 目标图相关的校验交给调用方（Walk 校具体图 / WalkRandom 校白名单交集）。
func (p *Payloads) walkBase() (*chainlib.Chain, error) {
	baseID := p.GhostBaseChainID()
	base, _, err := p.byIDVersioned(baseID)
	if err != nil {
		return nil, fmt.Errorf("游荡导航的基座链不可用（%s）：%w", baseID, err)
	}
	return base, nil
}

// cloneWalkChain 浅拷贝基座链（只带游荡要用的字段，不做任何裁剪）。
func cloneWalkChain(base *chainlib.Chain) *chainlib.Chain {
	return &chainlib.Chain{
		ChainID:  base.ChainID,
		GridCell: base.GridCell,
		NPCs:     base.NPCs,
		MapGrids: base.MapGrids,
		Dijkstra: base.Dijkstra,
		Maps:     base.Maps,
		Extra:    base.Extra,
	}
}

// gridKeysText 现有网格图号（升序、逗号分隔；报错文案用）。
func gridKeysText(base *chainlib.Chain) string {
	avail := make([]string, 0, len(base.MapGrids))
	for k := range base.MapGrids {
		avail = append(avail, k)
	}
	sort.Strings(avail)
	return strings.Join(avail, ",")
}

// NavOnly 该链文件是不是"导航数据"（没有任务节点 = task_order 为空）。
//
// 文件不存在返回 (false, nil)：那是「只下发 chain_id」的既有路径，不算误选；
// 解析失败返回 error（调用方决定怎么报）。
func (p *Payloads) NavOnly(chainID string) (bool, error) {
	chain, err := p.byID(chainID)
	if errors.Is(err, chainlib.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(chain.TaskOrder) == 0, nil
}

// ---------------------------------------------------------------- 组装与校验

// assembleGhostNav 基座 + 抓鬼专属 → 一份完整导航载荷。
//
// 合并规则（与参考实现 ghost_nav_payload 的字段集合一致；差异仅在于这里返回 chainlib.Chain，
// 序列化时会多带 start_task/end_task/task_hints/task_order 四个空值键——机器人端抓鬼不读它们，
// 任务链与抓鬼互斥且切回任务链会重新下发完整链数据）：
//   - npcs / map_grids / dijkstra：按 key 合并，专属文件优先（专属可以只补差量）；
//   - maps：取专属的（没有就沿用基座）；
//   - grid_cell / item_meta / 其它未知字段：基座打底，专属覆盖。
func assembleGhostNav(base, nav *chainlib.Chain) *chainlib.Chain {
	out := &chainlib.Chain{
		ChainID:  nav.ChainID,
		GridCell: base.GridCell,
		NPCs:     mergeRawMap(base.NPCs, nav.NPCs),
		MapGrids: mergeRawMap(base.MapGrids, nav.MapGrids),
		Dijkstra: mergeRawMap(base.Dijkstra, nav.Dijkstra),
		Maps:     base.Maps,
		Extra:    map[string]json.RawMessage{},
	}
	if out.ChainID == "" {
		out.ChainID = base.ChainID
	}
	if nav.GridCell != 0 {
		out.GridCell = nav.GridCell
	}
	if len(nav.Maps) > 0 {
		out.Maps = nav.Maps
	}
	// 基座里与抓鬼相关的"未知字段"（item_meta 等）先带上
	for k, v := range base.Extra {
		out.Extra[k] = v
	}
	// 专属文件覆盖（ghost_maps / ghost_map_pos / item_meta / …）
	for k, v := range nav.Extra {
		out.Extra[k] = v
	}
	// 基座若声明了 task_* 字段（本不该有），也不带给机器人：ghost 载荷只关心导航
	return out
}

// mergeRawMap 按 key 合并两个 RawMessage map（second 优先）；都为 nil 时返回空 map。
func mergeRawMap(first, second map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(first)+len(second))
	for k, v := range first {
		out[k] = v
	}
	for k, v := range second {
		out[k] = v
	}
	return out
}

// assembleShareDaily 基座 + 专属声明 → 一份完整分享日常链载荷。
//
// 合并规则（对比抓鬼：这里**保留任务链字段**——机器人端按 task_order 建"链内任务集合"）：
//   - npcs / map_grids / dijkstra：按 key 合并，专属优先（专属可只补差量）；
//   - task_order / task_hints / start_task / end_task / name：取专属（玩法声明；
//     task_order 绝不与基座新手链混用）；
//   - maps / grid_cell / item_meta 等：基座打底，专属覆盖。
func assembleShareDaily(base, nav *chainlib.Chain) *chainlib.Chain {
	out := &chainlib.Chain{
		ChainID:   nav.ChainID,
		Name:      nav.Name,
		StartTask: nav.StartTask,
		EndTask:   nav.EndTask,
		NPCs:      mergeRawMap(base.NPCs, nav.NPCs),
		TaskHints: mergeRawMap(base.TaskHints, nav.TaskHints),
		TaskOrder: nav.TaskOrder,
		GridCell:  base.GridCell,
		MapGrids:  mergeRawMap(base.MapGrids, nav.MapGrids),
		Dijkstra:  mergeRawMap(base.Dijkstra, nav.Dijkstra),
		Maps:      base.Maps,
		Extra:     map[string]json.RawMessage{},
	}
	if out.ChainID == "" {
		out.ChainID = base.ChainID
	}
	if nav.GridCell != 0 {
		out.GridCell = nav.GridCell
	}
	if len(nav.Maps) > 0 {
		out.Maps = nav.Maps
	}
	for k, v := range base.Extra {
		out.Extra[k] = v
	}
	for k, v := range nav.Extra {
		out.Extra[k] = v
	}
	return out
}

// validateShareDaily 分享日常载荷硬校验（与抓鬼同风格：宁可明确报错，不发"跑不动/少环节"的载荷）：
//
//  1. task_order 非空 —— 少列任务号会让后续环节被机器人当"链外任务"静默忽略（R1）；
//  2. task_order 每条必须能解析出 task_index（形状错=声明坏了，早点报错）；
//  3. npcs / dijkstra 非空（基座链选错或为空）。
func validateShareDaily(nav *chainlib.Chain, baseID, navID string) error {
	if len(nav.TaskOrder) == 0 {
		return fmt.Errorf("分享日常声明文件 %s 缺 task_order（必须列全该玩法全部分支任务号，"+
			"否则后续环节会被机器人当「链外任务」静默忽略）", navID)
	}
	bad := 0
	for _, raw := range nav.TaskOrder {
		var o map[string]any
		if err := json.Unmarshal(raw, &o); err != nil {
			bad++
			continue
		}
		if _, ok := o["task_index"]; !ok {
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("分享日常声明文件 %s 的 task_order 有 %d 条缺 task_index（形状不对）："+
			"每条应形如 {\"task_index\":2028301,\"catcher_npc\":\"13297\",\"next\":[...]}", navID, bad)
	}
	if len(nav.NPCs) == 0 {
		return fmt.Errorf("分享日常载荷缺 npcs（NPC 坐标）：基座链 %s 不对或者为空", baseID)
	}
	if len(nav.Dijkstra) == 0 {
		return fmt.Errorf("分享日常载荷缺 dijkstra（跨图路由）：基座链 %s 不对或者为空", baseID)
	}
	return nil
}

// validateGhostNav 地址完备性硬校验：宁可明确报错，也不要发一份"导航不了"的载荷。
//
// 必查（参考实现靠 XML+链数据保证，这里等价地校验一遍）：
//  1. npcs 非空（钟馗等 NPC 的坐标全在里面，机器人靠它找任务给予者）；
//  2. dijkstra 非空（跨图路由，否则刷鬼图之间走不过去）；
//  3. ghost_maps 非空（刷鬼图列表，机器人接取后要导航过去）；
//  4. 每个刷鬼图都在 map_grids 里（否则到图后只能直线走 = 穿墙/到不了鬼）；
//  5. ghost_map_pos 覆盖每个刷鬼图（筋斗云落点；参考实现由该图第一个 NPC 坐标推导）。
func validateGhostNav(nav *chainlib.Chain, baseID, navID string) error {
	if len(nav.NPCs) == 0 {
		return fmt.Errorf("抓鬼导航数据缺 npcs（NPC 坐标）：基座链 %s 不对或者为空", baseID)
	}
	if len(nav.Dijkstra) == 0 {
		return fmt.Errorf("抓鬼导航数据缺 dijkstra（跨图路由）：基座链 %s 不对或者为空", baseID)
	}
	ghostMaps, err := ghostMapsOf(nav)
	if err != nil {
		return err
	}
	if len(ghostMaps) == 0 {
		return fmt.Errorf("抓鬼导航数据缺 ghost_maps（刷鬼图列表）：请检查 %s（参考实现由 20195.xml 的「捉鬼npc」解析）", navID)
	}
	landings, err := ghostMapPos(nav)
	if err != nil {
		return err
	}
	missingGrid, missingPos := []string{}, []string{}
	for _, m := range ghostMaps {
		if _, ok := nav.MapGrids[strconv.Itoa(m)]; !ok {
			missingGrid = append(missingGrid, strconv.Itoa(m))
		}
		if _, ok := landings[strconv.Itoa(m)]; !ok {
			missingPos = append(missingPos, strconv.Itoa(m))
		}
	}
	if len(missingGrid) > 0 {
		sort.Strings(missingGrid)
		return fmt.Errorf("刷鬼图 %s 在 map_grids 里没有寻路网格（到图后会直线走/穿墙）：基座链 %s 的网格不含这些图，"+
			"请在 %s 里补上这几张图的 map_grids（参考实现会额外解析 70010/70011/70012/50101.xml 再合并）",
			strings.Join(missingGrid, ","), baseID, navID)
	}
	if len(missingPos) > 0 {
		sort.Strings(missingPos)
		return fmt.Errorf("刷鬼图 %s 缺 ghost_map_pos 落点（筋斗云传送要用）：请在 %s 里补上"+
			"（参考实现取该图第一个 NPC 坐标作落点）", strings.Join(missingPos, ","), navID)
	}
	return nil
}

// ghostMapsOf 读 Extra 里的 ghost_maps（数组，元素是数字或数字字符串）。
func ghostMapsOf(nav *chainlib.Chain) ([]int, error) {
	raw, ok := nav.Extra["ghost_maps"]
	if !ok {
		return nil, nil
	}
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("ghost_maps 形状不对（应是数组）: %w", err)
	}
	out := make([]int, 0, len(arr))
	for _, v := range arr {
		switch t := v.(type) {
		case float64:
			out = append(out, int(t))
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// ghostMapPos 读 Extra 里的 ghost_map_pos（对象：mapid → [x,y]）。
func ghostMapPos(nav *chainlib.Chain) (map[string]json.RawMessage, error) {
	raw, ok := nav.Extra["ghost_map_pos"]
	if !ok {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("ghost_map_pos 形状不对（应是对象：mapid → [x,y]）: %w", err)
	}
	return m, nil
}

// ---------------------------------------------------------------- 文件缓存

// byID 读 <ChainDir>/<id>.json（带缓存）。
func (p *Payloads) byID(chainID string) (*chainlib.Chain, error) {
	c, _, err := p.byIDVersioned(chainID)
	return c, err
}

// byIDVersioned 同上，另返回版本串（size:mtime）——组装结果按"两文件版本"缓存。
func (p *Payloads) byIDVersioned(chainID string) (*chainlib.Chain, string, error) {
	if p == nil || p.cfg == nil {
		return nil, "", errors.New("链载荷提供者未初始化")
	}
	id := strings.TrimSpace(chainID)
	if id == "" {
		return nil, "", errors.New("chain_id 为空")
	}
	path := chainlib.FilePath(id, p.cfg.ChainDir)
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("%w：%s", chainlib.ErrNotFound, path)
		}
		return nil, "", err
	}
	version := strconv.FormatInt(st.Size(), 10) + ":" + strconv.FormatInt(st.ModTime().UnixNano(), 10)

	p.mu.Lock()
	if c, ok := p.cache[id]; ok && c.version == version {
		p.mu.Unlock()
		return c.chain, version, nil
	}
	p.mu.Unlock()

	chain, err := chainlib.Build(id, p.cfg.ChainDir)
	if err != nil {
		return nil, "", err
	}
	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[string]cachedChain{}
	}
	p.cache[id] = cachedChain{version: version, chain: chain}
	p.mu.Unlock()
	return chain, version, nil
}
