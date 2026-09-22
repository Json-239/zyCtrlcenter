// Package zones 多区配置（两级：服 → 区），支撑「**单进程 + 切区**」：
//
//	服(Server)  = 一个游戏部署入口（IP + 默认编码）
//	  └ 区(Zone) = 该服下的一个入口（端口 + 可选编码覆盖）
//
// 只有**一个**机器人进程、**一条**控制通道：同一时刻只能登录一个区。
// 「切换当前区」= 选中目标区；要让机器人真正登录过去，用 ApplyRobotConfig 把该区的
// ip / port / PROTOCOL_CODING 写进机器人 config.py（写前自动备份），再重启机器人进程。
//
// 职责边界：本包只管**配置注册表**（增删改/切换/落盘/校验）与「把区作用到机器人 config.py」。
package zones

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound 服/区不存在。
var ErrNotFound = errors.New("服/区不存在")

// 协议编码取值（与机器人端 config.py 的 PROTOCOL_CODING 一致）。
//
// 当前环境统一使用 UTF-8：未显式指定编码时一律按 DefaultCoding（UTF-8）处理；
// GBK 仅作为个别区的可选覆盖值保留（历史/其它部署需要时可在区级指定）。
const (
	CodingGBK     = "GBK"
	CodingUTF8    = "UTF-8"
	DefaultCoding = CodingUTF8
)

// Zone 一个区（归属于某个服）。
type Zone struct {
	Key    string `json:"key"`              // 区内唯一；缺省用端口字符串（如 "2400"）
	Name   string `json:"name,omitempty"`   // 展示名，如 "1区"
	Port   int    `json:"port"`             // 游戏服端口（host 继承所属服）
	Coding string `json:"coding,omitempty"` // 覆盖服级编码；空=继承
	Note   string `json:"note,omitempty"`
}

// Server 一个服（游戏部署入口）。
type Server struct {
	Key    string `json:"key"`            // 服唯一标识，如 "prod-240"
	Name   string `json:"name,omitempty"` // 展示名，如 "生产网关"
	Host   string `json:"host"`           // 游戏服 IP（机器人端不支持域名）
	Coding string `json:"coding,omitempty"`
	Note   string `json:"note,omitempty"`
	Zones  []Zone `json:"zones"`
}

// Flat 展平后的区（全局唯一 key = 服key/区key），供接口/面板/写 config.py 消费。
type Flat struct {
	Key        string `json:"key"` // 全局唯一："<服key>/<区key>"
	ServerKey  string `json:"server_key"`
	ServerName string `json:"server_name,omitempty"`
	ZoneKey    string `json:"zone_key"`
	Name       string `json:"name,omitempty"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Coding     string `json:"coding"`
	Note       string `json:"note,omitempty"`
}

// Addr 游戏服地址（机器人连它）。
func (f Flat) Addr() string { return fmt.Sprintf("%s:%d", f.Host, f.Port) }

// DisplayName 展示名（缺省 "服/区"）。
func (f Flat) DisplayName() string {
	if strings.TrimSpace(f.Name) != "" {
		return f.Name
	}
	return f.Key
}

// MakeKey 区全局 key。
func MakeKey(serverKey, zoneKey string) string { return serverKey + "/" + zoneKey }

type cursor struct {
	Server string `json:"server"`
	Zone   string `json:"zone"`
}

type fileFormat struct {
	Current   cursor   `json:"current"`
	UpdatedAt int64    `json:"updated_at,omitempty"`
	Servers   []Server `json:"servers"`
}

// Registry 服/区注册表（并发安全，落盘 JSON）。
type Registry struct {
	path string

	mu        sync.Mutex
	current   cursor
	servers   []Server
	updatedAt int64
}

// New 创建注册表；path 通常为 <数据目录>/zones.json。
func New(path string) *Registry {
	return &Registry{path: path}
}

// Path 落盘路径。
func (r *Registry) Path() string { return r.path }

// Load 加载配置；文件不存在时写入默认种子（47.96.8.240 的 2400/2300 两个区）。返回是否用了种子。
func (r *Registry) Load() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	raw, err := os.ReadFile(r.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return false, err
		}
		r.servers = SeedServers()
		r.current = cursor{Server: r.servers[0].Key, Zone: r.servers[0].Zones[0].Key}
		if err := r.saveLocked(); err != nil {
			return true, err
		}
		return true, nil
	}

	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return false, fmt.Errorf("区配置 %s 解析失败: %w", r.path, err)
	}
	servers := make([]Server, 0, len(f.Servers))
	for _, s := range f.Servers {
		ns, err := NormalizeServer(s)
		if err != nil {
			continue // 脏数据跳过，不阻塞启动
		}
		servers = append(servers, ns)
	}
	r.servers = servers
	r.current = f.Current
	r.updatedAt = f.UpdatedAt
	if _, ok := r.flatLocked(r.current.Server, r.current.Zone); !ok {
		r.current = cursor{}
		if len(servers) > 0 && len(servers[0].Zones) > 0 {
			r.current = cursor{Server: servers[0].Key, Zone: servers[0].Zones[0].Key} // 当前区被删 → 回退第一个
		}
	}
	return false, nil
}

// Save 落盘。
func (r *Registry) Save() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveLocked()
}

func (r *Registry) saveLocked() error {
	raw, err := json.MarshalIndent(fileFormat{
		Current: r.current, UpdatedAt: time.Now().Unix(), Servers: r.servers,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return err
	}
	r.updatedAt = time.Now().Unix()
	return nil
}

// ---------------------------------------------------------------- 查询

// Servers 返回服列表（深拷贝）。
func (r *Registry) Servers() []Server {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneServers(r.servers)
}

// Flat 返回全部区（展平，稳定排序：先服后区）。
func (r *Registry) Flat() []Flat {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flatAllLocked()
}

// FlatOf 取指定区的展平信息。
func (r *Registry) FlatOf(serverKey, zoneKey string) (Flat, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flatLocked(serverKey, zoneKey)
}

// Current 返回当前区（可能为空）。
func (r *Registry) Current() (Flat, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flatLocked(r.current.Server, r.current.Zone)
}

// CurrentKeys 返回当前 (服key, 区key)。
func (r *Registry) CurrentKeys() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current.Server, r.current.Zone
}

// UpdatedAt 最近落盘时间（秒）。
func (r *Registry) UpdatedAt() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updatedAt
}

func (r *Registry) flatAllLocked() []Flat {
	out := make([]Flat, 0)
	for _, s := range r.servers {
		for _, z := range s.Zones {
			out = append(out, makeFlat(s, z))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ServerKey != out[j].ServerKey {
			return out[i].ServerKey < out[j].ServerKey
		}
		return out[i].Port < out[j].Port
	})
	return out
}

func (r *Registry) flatLocked(serverKey, zoneKey string) (Flat, bool) {
	for _, s := range r.servers {
		if s.Key != serverKey {
			continue
		}
		for _, z := range s.Zones {
			if z.Key == zoneKey {
				return makeFlat(s, z), true
			}
		}
	}
	return Flat{}, false
}

func makeFlat(s Server, z Zone) Flat {
	coding := z.Coding
	if coding == "" {
		coding = s.Coding
	}
	if coding == "" {
		coding = DefaultCoding
	}
	return Flat{
		Key: MakeKey(s.Key, z.Key), ServerKey: s.Key, ServerName: s.Name,
		ZoneKey: z.Key, Name: z.Name, Host: s.Host, Port: z.Port, Coding: coding, Note: z.Note,
	}
}

// ---------------------------------------------------------------- 变更

// Switch 切换当前区（必须存在）；落盘。
func (r *Registry) Switch(serverKey, zoneKey string) (Flat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.flatLocked(serverKey, zoneKey)
	if !ok {
		return Flat{}, fmt.Errorf("%w: %s/%s", ErrNotFound, serverKey, zoneKey)
	}
	r.current = cursor{Server: serverKey, Zone: zoneKey}
	return f, r.saveLocked()
}

// UpsertServer 新增/更新服（含其下区列表全量覆盖）；返回规整后的服与是否新增。
func (r *Registry) UpsertServer(s Server) (Server, bool, error) {
	ns, err := NormalizeServer(s)
	if err != nil {
		return Server{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	added := true
	for i := range r.servers {
		if r.servers[i].Key == ns.Key {
			r.servers[i] = ns
			added = false
			break
		}
	}
	if added {
		r.servers = append(r.servers, ns)
	}
	r.ensureCurrentLocked()
	return ns, added, r.saveLocked()
}

// UpsertZone 新增/更新某个服下的区；返回规整后的区与是否新增。
func (r *Registry) UpsertZone(serverKey string, z Zone) (Zone, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := -1
	for i := range r.servers {
		if r.servers[i].Key == serverKey {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Zone{}, false, fmt.Errorf("%w: 服 %s", ErrNotFound, serverKey)
	}
	nz, err := NormalizeZone(z)
	if err != nil {
		return Zone{}, false, err
	}
	added := true
	for j := range r.servers[idx].Zones {
		if r.servers[idx].Zones[j].Key == nz.Key {
			r.servers[idx].Zones[j] = nz
			added = false
			break
		}
	}
	if added {
		r.servers[idx].Zones = append(r.servers[idx].Zones, nz)
	}
	sortZones(r.servers[idx].Zones)
	r.ensureCurrentLocked()
	return nz, added, r.saveLocked()
}

// RemoveServer 删除服（连同其区）。
func (r *Registry) RemoveServer(serverKey string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := -1
	for i := range r.servers {
		if r.servers[i].Key == serverKey {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}
	r.servers = append(r.servers[:idx], r.servers[idx+1:]...)
	r.ensureCurrentLocked()
	return true, r.saveLocked()
}

// RemoveZone 删除区。
func (r *Registry) RemoveZone(serverKey, zoneKey string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	removed := false
	for i := range r.servers {
		if r.servers[i].Key != serverKey {
			continue
		}
		zs := r.servers[i].Zones
		for j := range zs {
			if zs[j].Key == zoneKey {
				r.servers[i].Zones = append(zs[:j], zs[j+1:]...)
				removed = true
				break
			}
		}
		break
	}
	if !removed {
		return false, nil
	}
	r.ensureCurrentLocked()
	return true, r.saveLocked()
}

// ensureCurrentLocked 当前区失效时回退到第一个区（调用方需持锁）。
func (r *Registry) ensureCurrentLocked() {
	if _, ok := r.flatLocked(r.current.Server, r.current.Zone); ok {
		return
	}
	r.current = cursor{}
	for _, s := range r.servers {
		for _, z := range s.Zones {
			r.current = cursor{Server: s.Key, Zone: z.Key}
			return
		}
	}
}

// ---------------------------------------------------------------- 校验/种子

// NormalizeServer 校验并规整服（含其下区）。
func NormalizeServer(s Server) (Server, error) {
	out := Server{
		Key:    strings.TrimSpace(s.Key),
		Name:   strings.TrimSpace(s.Name),
		Host:   strings.TrimSpace(s.Host),
		Coding: strings.ToUpper(strings.TrimSpace(s.Coding)),
		Note:   strings.TrimSpace(s.Note),
	}
	if out.Host == "" {
		return Server{}, errors.New("服 host 不能为空（游戏服 IP）")
	}
	if out.Coding == "" {
		out.Coding = DefaultCoding
	}
	if err := checkCoding(out.Coding); err != nil {
		return Server{}, err
	}
	if out.Key == "" {
		out.Key = slug(out.Name)
	}
	if out.Key == "" {
		out.Key = slug(out.Host)
	}
	if out.Key == "" {
		return Server{}, errors.New("服 key 不能为空")
	}
	if out.Name == "" {
		out.Name = out.Key
	}
	zones := make([]Zone, 0, len(s.Zones))
	seen := map[string]bool{}
	for _, z := range s.Zones {
		nz, err := NormalizeZone(z)
		if err != nil {
			return Server{}, fmt.Errorf("服 %s 的区不合法: %w", out.Key, err)
		}
		if seen[nz.Key] {
			continue // 重复区 key 去重
		}
		seen[nz.Key] = true
		zones = append(zones, nz)
	}
	sortZones(zones)
	out.Zones = zones
	return out, nil
}

// NormalizeZone 校验并规整区（不含服级信息）。
func NormalizeZone(z Zone) (Zone, error) {
	out := Zone{
		Key:    strings.TrimSpace(z.Key),
		Name:   strings.TrimSpace(z.Name),
		Port:   z.Port,
		Coding: strings.ToUpper(strings.TrimSpace(z.Coding)),
		Note:   strings.TrimSpace(z.Note),
	}
	if out.Port <= 0 || out.Port > 65535 {
		return Zone{}, fmt.Errorf("区端口非法: %d", out.Port)
	}
	if out.Coding != "" {
		if err := checkCoding(out.Coding); err != nil {
			return Zone{}, err
		}
	}
	if out.Key == "" {
		out.Key = fmt.Sprintf("%d", out.Port)
	}
	if out.Name == "" {
		out.Name = out.Key
	}
	return out, nil
}

func checkCoding(c string) error {
	if c != CodingGBK && c != CodingUTF8 {
		return fmt.Errorf("编码只支持 %s / %s，实际 %q", CodingGBK, CodingUTF8, c)
	}
	return nil
}

func sortZones(zs []Zone) {
	sort.Slice(zs, func(i, j int) bool { return zs[i].Port < zs[j].Port })
}

func cloneServers(in []Server) []Server {
	out := make([]Server, len(in))
	for i, s := range in {
		out[i] = s
		// 2026-09-22 修复: 空区列表必须保持"空数组"，不能变 nil。
		//   append([]Zone(nil), 空切片...) 返回 nil → JSON 序列化成 "zones": null →
		//   前端 `s.zones.length` 抛 TypeError → 「系统信息」页整页渲染崩溃
		//   （表现: 无法新增服/区、无法切换）。用 make 保证非 nil 空切片。
		out[i].Zones = append(make([]Zone, 0, len(s.Zones)), s.Zones...)
	}
	return out
}

// slug 生成稳定 key（字母数字与 - _ 保留，其余转 -）。
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == '.' || r == ':' || r == ' ':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// SeedServers 默认种子：生产服 47.96.8.240（UTF-8）下的 2400 / 2300 两个区 = 1 服多区。
func SeedServers() []Server {
	return []Server{
		{
			Key: "prod-240", Name: "生产网关", Host: "47.96.8.240", Coding: DefaultCoding,
			Note: "默认种子（可在面板改/删）",
			Zones: []Zone{
				{Key: "2400", Name: "1区", Port: 2400, Note: "默认种子区"},
				{Key: "2300", Name: "2区", Port: 2300, Note: "默认种子区"},
			},
		},
	}
}
