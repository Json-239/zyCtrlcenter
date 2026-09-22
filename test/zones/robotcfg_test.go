// robotcfg 测试：把区写进机器人 config.py —— 只改三个键、保留其它内容、写前备份。
package zones_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zyctrlcenter/internal/services/zones"
	"zyctrlcenter/test/testsupport"
)

// robotConfigFixture config.py 夹具结构。
type robotConfigFixture struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

// loadRobotConfigFixture 读取并校验 config.py 夹具（必须带 provenance.how）。
func loadRobotConfigFixture(t *testing.T, name string) robotConfigFixture {
	t.Helper()
	path := filepath.Join(testsupport.TestRoot(t), "fixtures", "robotcfg", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 config.py 夹具失败: %v", err)
	}
	var fx struct {
		Provenance map[string]any `json:"provenance"`
		robotConfigFixture
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("config.py 夹具非法: %v", err)
	}
	if how, _ := fx.Provenance["how"].(string); how == "" {
		t.Fatalf("config.py 夹具 %s 缺少 provenance.how（来源必须可查）", name)
	}
	return fx.robotConfigFixture
}

// writeRobotConfig 用夹具内容造一个假的机器人 config.py，返回其路径。
func writeRobotConfig(t *testing.T) string {
	t.Helper()
	fx := loadRobotConfigFixture(t, "config_py.sample.json")
	dir := filepath.Join(t.TempDir(), "script")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, fx.Filename)
	if err := os.WriteFile(out, []byte(fx.Content), 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestApplyRobotConfigRewritesOnlyThreeKeys(t *testing.T) {
	conf := writeRobotConfig(t)

	res, err := zones.ApplyRobotConfig(conf, "47.96.8.240", 2400, zones.DefaultCoding) // 默认 UTF-8
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !res.Applied {
		t.Fatal("值发生变化时应写盘")
	}
	if len(res.Changed) != 3 {
		t.Fatalf("应改动 ip/port/PROTOCOL_CODING 三个键，实际 %v", res.Changed)
	}
	if res.Backup == "" {
		t.Fatal("有改动时必须生成备份")
	}
	if _, err := os.Stat(res.Backup); err != nil {
		t.Fatalf("备份文件不存在: %v", err)
	}

	raw, _ := os.ReadFile(conf)
	content := string(raw)
	for _, want := range []string{`ip = "47.96.8.240"`, `port = 2400`, `PROTOCOL_CODING = "UTF-8"`} {
		if !strings.Contains(content, want) {
			t.Fatalf("写入结果缺少 %q\n%s", want, content)
		}
	}
	if strings.Contains(content, `PROTOCOL_CODING = "GBK"`) {
		t.Fatalf("旧编码应被改写为 UTF-8\n%s", content)
	}
	// 其它内容与注释必须原样保留（"只改三个键"）
	for _, keep := range []string{
		"# 机器人端配置样例（仅用于测试中控写入逻辑）",
		`PROTOCOL_VERSION = "11050911"`,
		"robot_count = 1",
		"robot_task_tester = True",
		"# 旧编码；当前环境统一为 UTF-8", // 行尾注释保留
	} {
		if !strings.Contains(content, keep) {
			t.Fatalf("不应改动/丢失的内容: %q\n%s", keep, content)
		}
	}
	// 备份内容 = 原文件
	backupRaw, _ := os.ReadFile(res.Backup)
	if !strings.Contains(string(backupRaw), `ip = "192.168.0.201"`) {
		t.Fatal("备份应是改动前的原文")
	}
}

func TestApplyRobotConfigNoChangeSkipsWrite(t *testing.T) {
	conf := writeRobotConfig(t)
	// 先真写一次（旧编码 GBK → 默认 UTF-8）
	first, err := zones.ApplyRobotConfig(conf, "47.96.8.240", 2400, zones.DefaultCoding)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Applied {
		t.Fatalf("首次应用应写盘（夹具是旧编码）：%+v", first)
	}
	before, _ := os.ReadFile(conf)

	// 再应用同样的值：不应写盘/备份
	res, err := zones.ApplyRobotConfig(conf, "47.96.8.240", 2400, zones.DefaultCoding)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied {
		t.Fatal("值已一致时不应写盘")
	}
	if len(res.Unchanged) != 3 || len(res.Changed) != 0 {
		t.Fatalf("应全部标记 unchanged，实际 changed=%v unchanged=%v", res.Changed, res.Unchanged)
	}
	if res.Backup != "" {
		t.Fatalf("无改动时不应产生备份，实际 %s", res.Backup)
	}
	after, _ := os.ReadFile(conf)
	if string(before) != string(after) {
		t.Fatal("无改动时文件内容不应变化")
	}
}

func TestApplyRobotConfigMissingKeyAppended(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "config.py")
	if err := os.WriteFile(conf, []byte("robot_count = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := zones.ApplyRobotConfig(conf, "1.2.3.4", 2300, zones.CodingUTF8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 3 {
		t.Fatalf("三个键都应记为 missing（已追加），实际 %v", res.Missing)
	}
	raw, _ := os.ReadFile(conf)
	content := string(raw)
	for _, want := range []string{`ip = "1.2.3.4"`, `port = 2300`, `PROTOCOL_CODING = "UTF-8"`, "robot_count = 1"} {
		if !strings.Contains(content, want) {
			t.Fatalf("追加结果缺少 %q\n%s", want, content)
		}
	}
}

// ⚠️ 安全约束：值是表达式（环境变量/函数调用）时**绝不改写**，只提示人工处理——
// 强行替换会把表达式改坏（如把 `int(_os.environ.get(...) or 2300)` 改成 `2400 or 2300)`）。
func TestApplyRobotConfigSkipsExpressionValues(t *testing.T) {
	fx := loadRobotConfigFixture(t, "config_py_env.sample.json")
	dir := t.TempDir()
	conf := filepath.Join(dir, "config.py")
	if err := os.WriteFile(conf, []byte(fx.Content), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := zones.ApplyRobotConfig(conf, "1.1.1.1", 2400, zones.CodingGBK)
	if err != nil {
		t.Fatalf("写入失败（应成功写入可改的部分）: %v", err)
	}
	// ip / port 是表达式 → 跳过；PROTOCOL_CODING 是字面量 → 正常改写
	if strings.Join(res.Skipped, ",") != "ip,port" {
		t.Fatalf("表达式键应被跳过，实际 skipped=%v changed=%v", res.Skipped, res.Changed)
	}
	if strings.Join(res.Changed, ",") != "PROTOCOL_CODING" {
		t.Fatalf("只应改写 PROTOCOL_CODING，实际 %v", res.Changed)
	}

	raw, _ := os.ReadFile(conf)
	content := string(raw)
	// 表达式行必须**原样保留**（一字不改）
	for _, keep := range []string{
		`ip = _os.environ.get("ROBOT_ZONE_IP") or "47.96.8.240"`,
		`port = int(_os.environ.get("ROBOT_ZONE_PORT") or 2300)`,
		`ctrl_server_port = int(_os.environ.get("ROBOT_CTRL_PORT") or 17200)`,
	} {
		if !strings.Contains(content, keep) {
			t.Fatalf("表达式行不应被改动: %q\n%s", keep, content)
		}
	}
	if !strings.Contains(content, `PROTOCOL_CODING = "GBK"`) {
		t.Fatalf("字面量键应被改写\n%s", content)
	}
	// 括号配对（防止改出语法错误）
	if strings.Count(content, "(") != strings.Count(content, ")") {
		t.Fatalf("改写后括号不配对（语法错误）\n%s", content)
	}
}

func TestApplyRobotConfigValidates(t *testing.T) {
	conf := writeRobotConfig(t)
	if _, err := zones.ApplyRobotConfig(filepath.Join(t.TempDir(), "nope.py"), "1.1.1.1", 2300, "GBK"); err == nil {
		t.Fatal("文件不存在必须报错（不新建，避免写错位置）")
	}
	if _, err := zones.ApplyRobotConfig(conf, "", 2300, "GBK"); err == nil {
		t.Fatal("host 为空必须报错")
	}
	if _, err := zones.ApplyRobotConfig(conf, "1.1.1.1", 0, "GBK"); err == nil {
		t.Fatal("port 非法必须报错")
	}
	if _, err := zones.ApplyRobotConfig(conf, "1.1.1.1", 2300, "utf8"); err == nil {
		t.Fatal("编码只接受 GBK / UTF-8（大小写敏感在上层规整）")
	}
}
