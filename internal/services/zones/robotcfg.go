package zones

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// 本文件实现「把某个区的地址写进机器人端 script/config.py」——
// 只改 ip / port / PROTOCOL_CODING 三个**字面量**赋值，其余内容（注释、其它配置、编码）原样保留；
// 写前自动备份，内容无变化时不写文件。
//
// 为什么这么小心：机器人端 config.py 是**生产配置文件**，误改会导致机器人连错服/乱码甚至语法错误。
//
// ⚠️ 安全约束（2026-09-19）：只改"字面量"值（`"1.2.3.4"` / `2300` / `"UTF-8"`）。
// 若某键的值是**表达式**（如 `ip = _os.environ.get("X") or "47.96.8.240"`），
// 一律**跳过不改**并记入 Skipped（提示人工处理）——强行替换会把表达式改坏（括号不配对等）。

// RobotConfigKeys 会被改写的三个键。
var RobotConfigKeys = []string{"ip", "port", "PROTOCOL_CODING"}

// RobotConfigResult 写入结果（面板展示 + 测试断言用）。
type RobotConfigResult struct {
	Path      string   `json:"path"`
	Backup    string   `json:"backup,omitempty"`    // 有改动时生成的备份文件（无改动为空）
	Changed   []string `json:"changed,omitempty"`   // 值发生变化的键
	Unchanged []string `json:"unchanged,omitempty"` // 值本来就一致的键
	Missing   []string `json:"missing,omitempty"`   // 文件里没有该键（已追加到末尾）
	Skipped   []string `json:"skipped,omitempty"`   // 值是表达式（非字面量）：未改动，需人工处理
	Applied   bool     `json:"applied"`             // 是否发生了写盘
}

// rule 一个键的匹配规则：assign 找到 "key ="，literal 从值位置起匹配字面量。
type rule struct {
	key     string
	assign  *regexp.Regexp
	literal *regexp.Regexp
}

func rules() []rule {
	return []rule{
		{"ip", regexp.MustCompile(`(?m)^([ \t]*ip[ \t]*=[ \t]*)`),
			regexp.MustCompile(`^("[^"]*"|'[^']*')`)},
		{"port", regexp.MustCompile(`(?m)^([ \t]*port[ \t]*=[ \t]*)`),
			regexp.MustCompile(`^\d+`)},
		{"PROTOCOL_CODING", regexp.MustCompile(`(?m)^([ \t]*PROTOCOL_CODING[ \t]*=[ \t]*)`),
			regexp.MustCompile(`^("[^"]*"|'[^']*')`)},
	}
}

// ApplyRobotConfig 把 (host, port, coding) 写入机器人 config.py。
//
// path 通常是 "<部署目录>/script/config.py"。文件不存在返回错误（不新建，避免写错位置）。
func ApplyRobotConfig(path, host string, port int, coding string) (RobotConfigResult, error) {
	res := RobotConfigResult{Path: path}
	if strings.TrimSpace(path) == "" {
		return res, errors.New("机器人 config.py 路径为空（部署目录未配置？）")
	}
	if strings.TrimSpace(host) == "" {
		return res, errors.New("host 为空")
	}
	if port <= 0 || port > 65535 {
		return res, fmt.Errorf("port 非法: %d", port)
	}
	if coding == "" {
		coding = DefaultCoding
	}
	if err := checkCoding(coding); err != nil {
		return res, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return res, fmt.Errorf("机器人配置文件不存在: %s", path)
		}
		return res, err
	}
	content := string(raw)
	original := content

	targets := map[string]string{
		"ip":              `"` + host + `"`,
		"port":            fmt.Sprintf("%d", port),
		"PROTOCOL_CODING": `"` + coding + `"`,
	}
	for _, rl := range rules() {
		newVal := targets[rl.key]
		loc := rl.assign.FindStringSubmatchIndex(content)
		if loc == nil {
			res.Missing = append(res.Missing, rl.key)
			continue
		}
		valueStart := loc[3] // "key =" 之后
		litIdx := rl.literal.FindStringIndex(content[valueStart:])
		if litIdx == nil || litIdx[0] != 0 {
			// 值是表达式/函数调用等非字面量：不动它（避免改坏），提示人工处理
			res.Skipped = append(res.Skipped, rl.key)
			continue
		}
		oldVal := content[valueStart : valueStart+litIdx[1]]
		if normalizeVal(oldVal) == normalizeVal(newVal) {
			res.Unchanged = append(res.Unchanged, rl.key)
			continue
		}
		start := valueStart
		end := valueStart + litIdx[1]
		content = content[:start] + newVal + content[end:]
		res.Changed = append(res.Changed, rl.key)
	}
	if len(res.Missing) > 0 {
		// 文件里缺键（如老版本配置）：追加到末尾，带注释标记，方便人工识别
		var b strings.Builder
		b.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n# === 由 zyctrlcenter 自动补充 ===\n")
		for _, k := range res.Missing {
			b.WriteString(k + " = " + targets[k] + "\n")
		}
		content = b.String()
	}

	if content == original {
		return res, nil // 无变化：不写盘、不备份
	}

	backup := fmt.Sprintf("%s.bak_%s", path, time.Now().Format("20060102_150405"))
	if err := copyFile(path, backup); err != nil {
		return res, fmt.Errorf("备份失败（未改动文件）: %w", err)
	}
	res.Backup = backup
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return res, fmt.Errorf("写入失败（备份在 %s）: %w", backup, err)
	}
	res.Applied = true
	return res, nil
}

// normalizeVal 去掉引号与空白，用于比较"值是否一致"。
func normalizeVal(v string) string {
	return strings.Trim(strings.TrimSpace(v), `"'`)
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o644)
}
