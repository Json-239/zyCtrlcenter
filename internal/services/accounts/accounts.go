// Package accounts 账号池：中控侧的账号账本（导入自 Python 中控账号库，运行期读写本文件）。
//
// 数据来源：`tools/import_accounts.py` 把 `E:\robot_accounts\robot_accounts.db` 导入成
// `<数据目录>/accounts.json`；本包读该文件做池子（列表/统计/选号/增删），
// 并把「某号在某区是否可用」等状态透给面板。
//
// 设计约定：
//   - **不改写源库**：中控只维护自己的 JSON 池；重新导入（跑 tools）会覆盖池文件；
//   - 外部重新导入后自动生效（按 mtime+size 热更新）；
//   - 每次变更立即原子落盘（tmp + rename），避免半截文件；
//   - 「在线/状态」不落盘：那是运行时信息，由 state（事件驱动）提供，API 层合并。
package accounts

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ZoneState 某账号在**某个区/服**上的状态（来自导入库的 account_server_state）。
type ZoneState struct {
	Verified  bool   `json:"verified"`
	Usable    bool   `json:"usable"`
	Msg       string `json:"msg,omitempty"`
	Level     int    `json:"level,omitempty"`
	RoleName  string `json:"role_name,omitempty"`
	ChainDone bool   `json:"chain_done,omitempty"`
	Fpp       int    `json:"fpp,omitempty"`
	Done      int    `json:"done,omitempty"`
	// VerifiedAt 最近一次"可用性验证"的时间（unix 秒；0 = 从未验证过）。
	VerifiedAt int64 `json:"verified_at,omitempty"`
	// Password 该区的登录密码（**建号时生成的随机密码写在这里**；登录/验证都从这里取）。
	// 空 = 该区没有密码，回退账号级 Password。
	Password string `json:"password,omitempty"`
}

// VerifyScope 批量验证的选号范围。
//
// 注意口径："该区的账号" = 池里**有该区状态记录**的账号（导入时来自 account_server_state）。
// 没记录的号不等于"不存在"，那属于 VerifyScopeUnknown（想探明"这个区到底有没有它"时才用）。
type VerifyScope string

const (
	// VerifyScopeUnverified 该区有记录、但还没验证过 / 验证没通过（**默认**：本地库当前区的待验证号）
	VerifyScopeUnverified VerifyScope = "unverified"
	// VerifyScopeUnusable 该区有记录、已验证但"存在却不可用"（重验看是否恢复）
	VerifyScopeUnusable VerifyScope = "unusable"
	// VerifyScopeUnknown 该区没有任何记录的号（探明"该区有没有它"，量大，按 limit 分批）
	VerifyScopeUnknown VerifyScope = "unknown"
	// VerifyScopeZone 该区**有记录的全部账号**（含已验证/不可用）——面板默认的"本区全部"
	VerifyScopeZone VerifyScope = "zone"
	// VerifyScopeAll 池内全部账号
	VerifyScopeAll VerifyScope = "all"
)

// Account 账号池条目。
type Account struct {
	Name       string                `json:"name"`
	Password   string                `json:"password,omitempty"`
	Level      int                   `json:"level,omitempty"`
	RoleName   string                `json:"role_name,omitempty"`
	ChainDone  bool                  `json:"chain_done,omitempty"`
	TaskType   string                `json:"task_type,omitempty"`
	Note       string                `json:"note,omitempty"`
	Fpp        int                   `json:"fpp,omitempty"`
	Done       int                   `json:"done,omitempty"`
	AddedAt    int64                 `json:"added_at,omitempty"`
	LastOnline int64                 `json:"last_online,omitempty"`
	Zones      map[string]*ZoneState `json:"zones,omitempty"` // zoneKey("host:port") → 状态
}

// Zone 取某区状态（不存在返回 nil）。
func (a *Account) Zone(zoneKey string) *ZoneState { return a.Zones[zoneKey] }

// UsableIn 该账号在指定区是否可用（未指定区时：任意一区可用即可）。
func (a *Account) UsableIn(zoneKey string) bool {
	if zoneKey == "" {
		for _, z := range a.Zones {
			if z != nil && z.Usable {
				return true
			}
		}
		return false
	}
	z := a.Zones[zoneKey]
	return z != nil && z.Usable
}

type fileFormat struct {
	Comment     string            `json:"_comment,omitempty"`
	GeneratedAt int64             `json:"generated_at,omitempty"`
	Source      string            `json:"source,omitempty"`
	Servers     map[string]string `json:"servers,omitempty"`
	Accounts    []*Account        `json:"accounts"`
}

// Filter 列表过滤条件（空值 = 不过滤）。
type Filter struct {
	Keyword string // 账号/角色名包含（忽略大小写）
	Zone    string // 该区有状态；配合 OnlyUsable 可筛"该区可用"
	Usable  bool   // 只看可用（按 Zone：Zone 为空则任意区可用）
	Limit   int    // 0 = 不限
	Offset  int
}

// Pool 账号池（并发安全 + 文件热更新）。
type Pool struct {
	path string

	mu          sync.Mutex
	accounts    map[string]*Account
	servers     map[string]string
	generatedAt int64
	source      string
	comment     string

	mtime time.Time
	size  int64
	ok    bool
	// dirty 有未落盘的内存改动（批量写回期间禁止热重载，否则改动会被冲掉）
	dirty bool
	// batch 正在"批量写窗口"里（Update 内）：窗口内一律不重载
	batch bool
	// rnd 选号平局用的随机源（2026-09-23 选号轮换）：同一 last_online 的号靠它打散顺序，
	// 避免"每次补号都从同一小批里挑"。nil = 用 math/rand；测试用 SetRand 注入固定序列。
	rnd func(int) int
}

// New 创建账号池（path 通常为 <数据目录>/accounts.json）。
func New(path string) *Pool {
	return &Pool{path: path, accounts: map[string]*Account{}, servers: map[string]string{}}
}

// Path 池文件路径。
func (p *Pool) Path() string { return p.path }

// SetRand 注入选号平局用的随机源（测试固定随机用；nil = 恢复默认 math/rand）。
func (p *Pool) SetRand(fn func(int) int) {
	p.mu.Lock()
	p.rnd = fn
	p.mu.Unlock()
}

// Load 读取池文件（不存在视为空池，不报错）。
func (p *Pool) Load() error {
	raw, err := os.ReadFile(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			p.mu.Lock()
			p.accounts, p.servers, p.ok = map[string]*Account{}, map[string]string{}, false
			p.mu.Unlock()
			return nil
		}
		return err
	}
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("账号池 %s 解析失败: %w", p.path, err)
	}
	m := make(map[string]*Account, len(f.Accounts))
	for _, a := range f.Accounts {
		if a == nil || a.Name == "" {
			continue
		}
		if a.Zones == nil {
			a.Zones = map[string]*ZoneState{}
		}
		m[a.Name] = a
	}
	st, _ := os.Stat(p.path)
	p.mu.Lock()
	p.accounts = m
	p.servers = f.Servers
	if p.servers == nil {
		p.servers = map[string]string{}
	}
	p.generatedAt, p.source, p.comment = f.GeneratedAt, f.Source, f.Comment
	p.ok = true
	if st != nil {
		p.mtime, p.size = st.ModTime(), st.Size()
	}
	p.mu.Unlock()
	return nil
}

// refresh 文件被外部改动（重新导入）时自动重载。
func (p *Pool) refresh() {
	// 有未落盘的内存改动时**不重载**：批量写回（SetZoneStateNoSave）期间，
	// 别的调用触发的 refresh 不能把还没 Save 的改动冲掉（会被"建号/验证"批量写丢密码）。
	p.mu.Lock()
	busy := p.dirty || p.batch
	p.mu.Unlock()
	if busy {
		return
	}
	st, err := os.Stat(p.path)
	if err != nil {
		return
	}
	p.mu.Lock()
	same := p.ok && st.ModTime().Equal(p.mtime) && st.Size() == p.size
	p.mu.Unlock()
	if !same {
		_ = p.Load()
	}
}

// BeginBatch 进入"批量写窗口"：窗口内**禁止热重载**（否则并发写回会被重载冲掉）。
// 必须与 EndBatch 配对（跨 goroutine 的批处理用它；单线程小批量可用 Update）。
func (p *Pool) BeginBatch() {
	p.mu.Lock()
	p.batch = true
	p.mu.Unlock()
}

// EndBatch 结束批量写窗口并落盘一次。
func (p *Pool) EndBatch() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.batch = false
	return p.saveLocked()
}

// Update 在"批量写窗口"里执行 fn，结束后统一落盘（等价 BeginBatch + fn + EndBatch）。
func (p *Pool) Update(fn func()) error {
	p.BeginBatch()
	defer func() { p.mu.Lock(); p.batch = false; p.mu.Unlock() }()
	fn()
	return p.EndBatch()
}

// Save 原子落盘。
func (p *Pool) Save() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saveLocked()
}

func (p *Pool) saveLocked() error {
	list := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	raw, err := json.MarshalIndent(fileFormat{
		Comment:     p.comment,
		GeneratedAt: p.generatedAt,
		Source:      p.source,
		Servers:     p.servers,
		Accounts:    list,
	}, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return err
	}
	if st, err := os.Stat(p.path); err == nil {
		p.mtime, p.size = st.ModTime(), st.Size()
	}
	p.ok = true
	p.dirty = false
	return nil
}

// ---------------------------------------------------------------- 查询

// Meta 池元信息（面板展示"数据何时导入的"）。
func (p *Pool) Meta() map[string]any {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	return map[string]any{
		"path":         p.path,
		"loaded":       p.ok,
		"count":        len(p.accounts),
		"generated_at": p.generatedAt,
		"imported_at":  p.generatedAt, // 兼容字段名
		"source":       p.source,
		"servers":      copyServers(p.servers),
		"file_mtime":   p.mtime.Unix(),
	}
}

func copyServers(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Get 取账号（返回副本，调用方可安全读取）。
func (p *Pool) Get(name string) (*Account, bool) {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.accounts[name]
	if !ok {
		return nil, false
	}
	return cloneAccount(a), true
}

// Password 取账号密码（不在池里或密码为空时返回 def）。
// Usable 该号在指定区是否已验证可用（zone = "<host:port>"；空 = 任意区可用）。
func (p *Pool) Usable(name, zone string) bool {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.accounts[name]
	if !ok {
		return false
	}
	return a.UsableIn(zone)
}

func (p *Pool) Password(name, zone string) string {
	pw, _ := p.PasswordFor(name, zone)
	return pw
}

// PasswordFor 取某账号在某区登录用的密码，并告知"库里到底有没有"。
//
// 优先级：**区级**（建号生成的随机密码写这里）→ 账号级（导入库里的老数据）。
// 查不到返回 ok=false —— 调用方**不得**用"统一/默认密码"顶替（否则会用错密码反复登录失败）。
func (p *Pool) PasswordFor(name, zone string) (string, bool) {
	if name == "" {
		return "", false
	}
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.accounts[name]
	if !ok {
		return "", false
	}
	// 1) 精确到区
	if z := a.Zones[zone]; z != nil && strings.TrimSpace(z.Password) != "" {
		return z.Password, true
	}
	// 2) 同一个"服"（同 host 的其它区）—— 密码按服通用
	if host := hostOf(zone); host != "" {
		best := ""
		for k, z := range a.Zones {
			if z == nil || strings.TrimSpace(z.Password) == "" || hostOf(k) != host {
				continue
			}
			if best == "" || k < best { // 取 key 最小的那个，保证结果稳定
				best = k
			}
		}
		if best != "" {
			return a.Zones[best].Password, true
		}
	}
	// 3) 账号级（导入库里的老数据）
	if strings.TrimSpace(a.Password) != "" {
		return a.Password, true
	}
	return "", false
}

// hostOf 取 "<host>:<port>" 的 host 部分（没有端口则原样返回；空返回空）。
func hostOf(zone string) string {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return ""
	}
	if i := strings.LastIndex(zone, ":"); i > 0 {
		return zone[:i]
	}
	return zone
}

// Count 池内账号数。
func (p *Pool) Count() int {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accounts)
}

// List 按过滤条件列出（按账号名升序；分页在过滤之后应用）。
func (p *Pool) List(f Filter) []*Account {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	kw := strings.ToLower(strings.TrimSpace(f.Keyword))
	out := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if kw != "" && !strings.Contains(strings.ToLower(a.Name), kw) &&
			!strings.Contains(strings.ToLower(a.RoleName), kw) {
			continue
		}
		if f.Zone != "" && a.Zones[f.Zone] == nil {
			continue
		}
		if f.Usable && !a.UsableIn(f.Zone) {
			continue
		}
		out = append(out, cloneAccount(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if f.Offset > 0 {
		if f.Offset >= len(out) {
			return []*Account{}
		}
		out = out[f.Offset:]
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

// Stats 统计：总数 / 指定区可用 / 指定区已分配（有状态）。
func (p *Pool) Stats(zone string) map[string]int {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int{"total": len(p.accounts), "usable": 0, "assigned": 0, "chain_done": 0}
	for _, a := range p.accounts {
		if zone == "" {
			if a.UsableIn("") {
				out["usable"]++
			}
		} else if z := a.Zones[zone]; z != nil {
			out["assigned"]++
			if z.Usable {
				out["usable"]++
			}
		}
		if a.ChainDone {
			out["chain_done"]++
		}
	}
	return out
}

// Pick 选号：从池里挑 limit 个（可按区可用过滤），用于批量上线。
// exclude 里的账号会被跳过（例如已经在线/已选过的）。
//
// 排序口径（2026-09-23 选号轮换；旧实现是"按账号名升序取前 N"—— 会让名字最前的那批号被
// 反复选中，新手号/号段靠后的号永远轮不到）：
//  1. 从没上过线（LastOnline<=0）排最前；
//  2. 其余按 LastOnline 升序（最久没上的优先）；
//  3. 同值用随机源打破平局（先整体洗牌再稳定排序 → 平局组内顺序随机）。
//
// LastOnline 的维护见 TouchOnline（上线下发成功后回写；不更新会退化成"固定一批号"）。
func (p *Pool) Pick(zone string, limit int, onlyUsable bool, exclude map[string]bool) []string {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	cands := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if exclude[a.Name] {
			continue
		}
		if onlyUsable && !a.UsableIn(zone) {
			continue
		}
		cands = append(cands, a)
	}
	shuffleAccounts(cands, p.rndOr())
	sort.SliceStable(cands, func(i, j int) bool {
		return onlineLess(cands[i].LastOnline, cands[j].LastOnline)
	})
	if limit > 0 && len(cands) > limit {
		cands = cands[:limit]
	}
	out := make([]string, 0, len(cands))
	for _, a := range cands {
		out = append(out, a.Name)
	}
	return out
}

// TouchOnline 记"这些号刚被下发上线"（LastOnline=ts，unix 秒；只往大改，防时钟回拨）——
// 供选号轮换按"最久未上线优先"挑号（见 Pick）。池里没有的号跳过（保持"池 = 导入/显式添加"的
// 口径）；整批一次落盘（不按号逐个写盘）。
//
// 2026-09-23 补的**唯一运行期写入点**：此前 last_online 只由 tools/import_accounts.py 在导入时
// 拷贝一次（源库那侧最后一次批量更新停在 2026-09-10/11），运行期没有任何写入 —— 不补的话
// "最久未上线优先"会退化成静态顺序（同一批号被反复拉起、压下去又被立刻补回来）。
func (p *Pool) TouchOnline(names []string, ts int64) int {
	if ts <= 0 || len(names) == 0 {
		return 0
	}
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, name := range names {
		a, ok := p.accounts[name]
		if !ok {
			continue
		}
		if a.LastOnline < ts {
			a.LastOnline = ts
		}
		n++
	}
	if n > 0 {
		p.dirty = true
		_ = p.saveLocked()
	}
	return n
}

// ---------------------------------------------------------------- 选号轮换小工具

// onlineLess 选号排序：从没上过线（<=0）最前；其余按 last_online 升序（最久没上的优先）。
func onlineLess(a, b int64) bool {
	if a <= 0 || b <= 0 {
		return a <= 0 && b > 0
	}
	return a < b
}

// rndOr 当前随机源（未注入时用 math/rand；并发安全，全局源自带锁）。
func (p *Pool) rndOr() func(int) int {
	if p.rnd != nil {
		return p.rnd
	}
	return func(n int) int {
		if n <= 0 {
			return 0
		}
		return rand.Intn(n)
	}
}

// shuffleAccounts 原地 Fisher-Yates（rnd=nil 时不动：保持输入顺序的确定性）。
func shuffleAccounts(accs []*Account, rnd func(int) int) {
	if rnd == nil {
		return
	}
	for i := len(accs) - 1; i > 0; i-- {
		j := rnd(i + 1)
		if j < 0 || j > i {
			j = i // 防呆：随机源给了越界值也不出错
		}
		accs[i], accs[j] = accs[j], accs[i]
	}
}

// SelectForVerify 选出一批**待验证**账号（账号名升序；exclude 里的跳过，例如"正在线跑着"的号）。
//
// 范围语义（zone = "<host:port>"，即选中区的账号）：
//   - VerifyScopeUnverified：该区有记录且 verified=false（**默认**，本地库当前区的待验证号）
//   - VerifyScopeUnusable  ：该区有记录、已验证但 usable=false（重验看是否恢复）
//   - VerifyScopeUnknown   ：该区没有任何记录
//   - VerifyScopeAll       ：池内全部
//
// 只返回账号名（密码由调用方按池取，不下发到前端）；limit<=0 表示不限。
func (p *Pool) SelectForVerify(zone string, scope VerifyScope, limit int, exclude map[string]bool) []string {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()

	names := make([]string, 0, len(p.accounts))
	for _, a := range p.accounts {
		if exclude[a.Name] {
			continue
		}
		z := a.Zones[zone]
		switch scope {
		case VerifyScopeAll:
			// 全要
		case VerifyScopeUnknown:
			if z != nil {
				continue
			}
		case VerifyScopeUnusable:
			if z == nil || !z.Verified || z.Usable {
				continue
			}
		case VerifyScopeZone:
			if z == nil {
				continue
			}
		default: // VerifyScopeUnverified
			if z == nil || z.Verified {
				continue
			}
		}
		names = append(names, a.Name)
	}
	sort.Strings(names)
	if limit > 0 && len(names) > limit {
		names = names[:limit]
	}
	return names
}

// CountForVerify 待验证数量预览（面板显示"当前区还有多少个待验证"）。
func (p *Pool) CountForVerify(zone string) map[string]int {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int{"unverified": 0, "unusable": 0, "unknown": 0, "zone": 0, "all": len(p.accounts)}
	for _, a := range p.accounts {
		z := a.Zones[zone]
		if z != nil {
			out["zone"]++ // 本区有记录的全部（面板默认范围）
		}
		switch {
		case z == nil:
			out["unknown"]++
		case !z.Verified:
			out["unverified"]++
		case !z.Usable:
			out["unusable"]++
		}
	}
	return out
}

// ---------------------------------------------------------------- 变更

// Add 批量加号（已存在则只补空密码/区状态；返回新增与已存在清单）。
func (p *Pool) Add(names []string, password, zone, note string) (added, existed []string) {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dirty = true
	now := time.Now().Unix()
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		a, ok := p.accounts[n]
		if !ok {
			a = &Account{Name: n, AddedAt: now, Zones: map[string]*ZoneState{}}
			p.accounts[n] = a
			added = append(added, n)
		} else {
			existed = append(existed, n)
		}
		if password != "" && a.Password == "" {
			a.Password = password
		}
		if note != "" {
			a.Note = note
		}
		if zone != "" {
			if a.Zones[zone] == nil {
				a.Zones[zone] = &ZoneState{}
			}
		}
	}
	_ = p.saveLocked()
	return added, existed
}

// Remove 批量删号，返回删除数量。
func (p *Pool) Remove(names []string) int {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dirty = true
	n := 0
	for _, name := range names {
		if _, ok := p.accounts[name]; ok {
			delete(p.accounts, name)
			n++
		}
	}
	if n > 0 {
		_ = p.saveLocked()
	}
	return n
}

// SetZoneState 写入/更新某账号在某区的状态（验证结果、等级、链完成等），并**立即落盘**。
func (p *Pool) SetZoneState(name, zone string, st ZoneState) {
	if name == "" || zone == "" {
		return
	}
	p.setZoneStateLocked(name, zone, st)
	p.mu.Lock()
	_ = p.saveLocked()
	p.mu.Unlock()
}

// SetZoneStateNoSave 同 SetZoneState，但**不落盘**：批量验证时逐条写内存，
// 由调用方每若干条（或整批结束）调一次 Save() —— 否则 5000 个号要写 5000 次文件。
func (p *Pool) SetZoneStateNoSave(name, zone string, st ZoneState) {
	if name == "" || zone == "" {
		return
	}
	p.setZoneStateLocked(name, zone, st)
}

func (p *Pool) setZoneStateLocked(name, zone string, st ZoneState) {
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dirty = true
	a, ok := p.accounts[name]
	if !ok {
		a = &Account{Name: name, AddedAt: time.Now().Unix(), Zones: map[string]*ZoneState{}}
		p.accounts[name] = a
	}
	if a.Zones == nil {
		a.Zones = map[string]*ZoneState{}
	}
	a.Zones[zone] = &st
}

// SetFields 更新账号级字段（等级/角色/链完成等，来自心跳回写）。
func (p *Pool) SetFields(name string, fn func(a *Account)) {
	if name == "" || fn == nil {
		return
	}
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dirty = true
	a, ok := p.accounts[name]
	if !ok {
		return // 不在池里的号不自动建（保持"池 = 导入/显式添加"的口径）
	}
	fn(a)
	_ = p.saveLocked()
}

func cloneAccount(a *Account) *Account {
	out := *a
	if a.Zones != nil {
		out.Zones = make(map[string]*ZoneState, len(a.Zones))
		for k, v := range a.Zones {
			if v == nil {
				out.Zones[k] = nil
				continue
			}
			zs := *v
			out.Zones[k] = &zs
		}
	}
	return &out
}
