// zones 测试：种子 / 切换持久化 / 增删改 / 校验 / 展平与编码继承（单进程：切区用）。
package zones_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"zyctrlcenter/internal/services/zones"
	"zyctrlcenter/test/testsupport"
)

func TestSeedOnFirstLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zones.json")
	reg := zones.New(path)

	seeded, err := reg.Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !seeded {
		t.Fatal("文件不存在时应写入种子")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("种子应落盘: %v", err)
	}

	servers := reg.Servers()
	if len(servers) != 1 || servers[0].Host != "47.96.8.240" {
		t.Fatalf("种子服不符: %+v", servers)
	}
	if len(servers[0].Zones) != 2 {
		t.Fatalf("种子应有 2 个区（2400/2300），实际 %+v", servers[0].Zones)
	}
	flat := reg.Flat()
	if len(flat) != 2 {
		t.Fatalf("应展平出 2 个区，实际 %+v", flat)
	}
	byKey := map[string]zones.Flat{}
	for _, f := range flat {
		byKey[f.Key] = f
	}
	zone2400, ok := byKey["prod-240/2400"]
	if !ok {
		t.Fatalf("应展平出 prod-240/2400，实际 %+v", flat)
	}
	if zone2400.Host != "47.96.8.240" || zone2400.Coding != "UTF-8" || zone2400.Port != 2400 {
		t.Fatalf("展平后的区字段不符（默认编码应为 UTF-8）: %+v", zone2400)
	}
	if zone2400.ServerName != "生产网关" || zone2400.ZoneKey != "2400" || zone2400.Addr() != "47.96.8.240:2400" {
		t.Fatalf("展平后的服/区字段不符: %+v", zone2400)
	}
	if _, ok := reg.Current(); !ok {
		t.Fatal("种子后应自动选中一个当前区")
	}

	// 二次加载不应再播种
	reg2 := zones.New(path)
	seeded2, err := reg2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if seeded2 {
		t.Fatal("已有配置文件时不应重复播种")
	}
	if len(reg2.Flat()) != 2 {
		t.Fatalf("二次加载区数不符: %+v", reg2.Flat())
	}
}

func TestSwitchPersistsAndValidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zones.json")
	reg := zones.New(path)
	if _, err := reg.Load(); err != nil {
		t.Fatal(err)
	}

	flat, err := reg.Switch("prod-240", "2300")
	if err != nil {
		t.Fatalf("切换失败: %v", err)
	}
	if flat.Key != "prod-240/2300" || flat.Port != 2300 {
		t.Fatalf("切换返回的区不符: %+v", flat)
	}
	if sk, zk := reg.CurrentKeys(); sk != "prod-240" || zk != "2300" {
		t.Fatalf("当前区未更新: %s/%s", sk, zk)
	}

	// 重新加载（模拟重启）：当前区应保持
	reg2 := zones.New(path)
	if _, err := reg2.Load(); err != nil {
		t.Fatal(err)
	}
	if _, zk := reg2.CurrentKeys(); zk != "2300" {
		t.Fatalf("当前区未持久化，实际 %s", zk)
	}

	// 不存在的区
	if _, err := reg.Switch("prod-240", "nope"); !errors.Is(err, zones.ErrNotFound) {
		t.Fatalf("不存在的区应返回 ErrNotFound，实际 %v", err)
	}
}

func TestUpsertZoneAndUpdate(t *testing.T) {
	reg := testsupport.NewTestZones(t)

	// 新增：区 key 缺省 = 端口字符串
	added, isNew, err := reg.UpsertZone(testsupport.TestServerKey, zones.Zone{Name: "新3区", Port: 2400})
	if err != nil {
		t.Fatalf("新增区失败: %v", err)
	}
	if !isNew {
		t.Fatal("应标记为新增")
	}
	if added.Key != "2400" {
		t.Fatalf("区 key 缺省应为端口字符串，实际 %q", added.Key)
	}

	// 更新同 key：不新增、字段生效
	upd, isNew2, err := reg.UpsertZone(testsupport.TestServerKey, zones.Zone{
		Key: "2400", Name: "改名后", Port: 2400, Coding: "GBK", // 编码可覆盖（默认 UTF-8）
	})
	if err != nil {
		t.Fatal(err)
	}
	if isNew2 {
		t.Fatal("同 key 应为更新而非新增")
	}
	if upd.Name != "改名后" || upd.Coding != "GBK" {
		t.Fatalf("更新未生效: %+v", upd)
	}
	if len(reg.Flat()) != 2 {
		t.Fatalf("区数应为 2，实际 %d", len(reg.Flat()))
	}
	// 展平后该区编码为覆盖值（未覆盖的区继承服级 UTF-8）
	for _, f := range reg.Flat() {
		if f.ZoneKey == "2400" && f.Coding != "GBK" {
			t.Fatalf("区级编码覆盖未生效: %+v", f)
		}
		if f.ZoneKey == "z1" && f.Coding != "UTF-8" {
			t.Fatalf("未覆盖的区应继承服级编码: %+v", f)
		}
	}
}

func TestRemoveZoneAndServerFallbackCurrent(t *testing.T) {
	reg := testsupport.NewTestZones(t)
	if _, _, err := reg.UpsertZone(testsupport.TestServerKey, zones.Zone{Key: "2400", Port: 2400}); err != nil {
		t.Fatal(err)
	}
	// 当前区 = z1；删掉当前区 → 回退到剩余第一个
	removed, err := reg.RemoveZone(testsupport.TestServerKey, "z1")
	if err != nil || !removed {
		t.Fatalf("删除区失败: removed=%v err=%v", removed, err)
	}
	if _, zk := reg.CurrentKeys(); zk != "2400" {
		t.Fatalf("当前区应回退到剩余区，实际 %q", zk)
	}
	// 删除不存在的区：不报错、removed=false
	if removed, err := reg.RemoveZone(testsupport.TestServerKey, "nope"); err != nil || removed {
		t.Fatalf("删除不存在的区应静默: removed=%v err=%v", removed, err)
	}
	// 删服 → 其区一并消失
	removed, err = reg.RemoveServer(testsupport.TestServerKey)
	if err != nil || !removed {
		t.Fatalf("删除服失败: %v", err)
	}
	if len(reg.Flat()) != 0 {
		t.Fatalf("删服后不应残留区: %+v", reg.Flat())
	}
	if _, ok := reg.Current(); ok {
		t.Fatal("无区时当前区应为空")
	}
}

func TestNormalizeValidation(t *testing.T) {
	if zones.DefaultCoding != "UTF-8" {
		t.Fatalf("默认编码应为 UTF-8，实际 %q", zones.DefaultCoding)
	}
	// 未指定编码 → 取默认（UTF-8）
	def, err := zones.NormalizeServer(zones.Server{Key: "s1", Host: "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	if def.Coding != "UTF-8" {
		t.Fatalf("未指定编码时应取默认 UTF-8，实际 %q", def.Coding)
	}
	if _, err := zones.NormalizeServer(zones.Server{Host: "", Zones: []zones.Zone{{Port: 2300}}}); err == nil {
		t.Fatal("服 host 为空必须报错")
	}
	if _, err := zones.NormalizeZone(zones.Zone{Port: 0}); err == nil {
		t.Fatal("区端口非法必须报错")
	}
	if _, err := zones.NormalizeZone(zones.Zone{Port: 2300, Coding: "UTF8"}); err == nil {
		t.Fatal("编码只支持 GBK/UTF-8")
	}
	z, err := zones.NormalizeZone(zones.Zone{Port: 2300, Name: "  1区  "})
	if err != nil {
		t.Fatal(err)
	}
	if z.Key != "2300" || z.Name != "1区" || z.Coding != "" {
		t.Fatalf("缺省 key 应为端口、展示名应去空白、编码留空以继承服: %+v", z)
	}
	// 服内重复区 key 去重
	s, err := zones.NormalizeServer(zones.Server{Key: "s1", Host: "1.2.3.4", Zones: []zones.Zone{
		{Key: "a", Port: 2300}, {Key: "a", Port: 2400},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Zones) != 1 {
		t.Fatalf("重复区 key 应去重，实际 %+v", s.Zones)
	}
}

func TestFlatInheritsServerCoding(t *testing.T) {
	reg := testsupport.NewTestZones(t) // 服 coding=UTF-8，区未指定
	flat, ok := reg.FlatOf(testsupport.TestServerKey, "z1")
	if !ok {
		t.Fatal("应能取到测试区")
	}
	if flat.Coding != "UTF-8" {
		t.Fatalf("区未指定编码时应继承服，实际 %s", flat.Coding)
	}
	if flat.Addr() != "127.0.0.1:2300" {
		t.Fatalf("区地址应为 host:port，实际 %s", flat.Addr())
	}

	// 区级覆盖编码
	if _, _, err := reg.UpsertZone(testsupport.TestServerKey, zones.Zone{
		Key: "z1", Port: 2300, Coding: "GBK",
	}); err != nil {
		t.Fatal(err)
	}
	flat, _ = reg.FlatOf(testsupport.TestServerKey, "z1")
	if flat.Coding != "GBK" {
		t.Fatalf("区级编码应覆盖服级，实际 %s", flat.Coding)
	}
}
