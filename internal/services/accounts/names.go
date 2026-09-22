// 按"编号"生成账号名：前缀 + 序号(补零) + 邮箱后缀（服务端注册要求邮箱格式）。
//
// 参考 py 中控的用法：填【前缀 robot000 / 起始序号 3004 / 数量 5 / 后缀 @xy3.com】
// 就能生成 robot0003004@xy3.com … robot0003008@xy3.com；起始序号还能"自动接续"
// （取池内同前缀最大序号 +1），避免撞上已有账号。
package accounts

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MaxCreateNames 单次按编号生成的账号数上限（注册是同 IP 写库操作，别一次太多）。
const MaxCreateNames = 200

// NameSpec 账号名生成规则。
type NameSpec struct {
	Prefix    string `json:"prefix"`     // 如 "robot000"（自带前导零就原样拼）
	Start     int    `json:"start"`      // 起始序号
	Count     int    `json:"count"`      // 生成个数
	Pad       int    `json:"pad"`        // 序号补零宽度（0=自动按最大序号位数）
	Suffix    string `json:"suffix"`     // 邮箱后缀，如 "@xy3.com"（可空）
	AutoStart bool   `json:"auto_start"` // 起始序号自动接续（池内该前缀最大序号+1）
}

// Validate 校验参数（错误信息直接给面板显示）。
func (s NameSpec) Validate() error {
	if strings.TrimSpace(s.Prefix) == "" {
		return fmt.Errorf("账号前缀不能为空")
	}
	if s.Count <= 0 {
		return fmt.Errorf("数量必须 > 0")
	}
	if s.Count > MaxCreateNames {
		return fmt.Errorf("数量超上限（一次最多 %d 个）", MaxCreateNames)
	}
	if s.Start < 0 {
		return fmt.Errorf("起始序号不能为负")
	}
	if s.Pad < 0 || s.Pad > 12 {
		return fmt.Errorf("补零宽度应在 0~12 之间")
	}
	return nil
}

// NextSeq 池内同前缀账号的"下一个可用序号"（没有历史则返回 0）。
func (p *Pool) NextSeq(prefix, suffix string) int {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return 0
	}
	re := regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + `(\d+)$`)
	p.refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	max := 0
	for name := range p.accounts {
		base := name
		if suffix != "" {
			if !strings.HasSuffix(base, suffix) {
				continue
			}
			base = strings.TrimSuffix(base, suffix)
		} else if i := strings.Index(base, "@"); i > 0 {
			base = base[:i] // 兼容带邮箱后缀的历史账号
		}
		m := re.FindStringSubmatch(base)
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil && n > max {
			max = n
		}
	}
	return max
}

// BuildNames 按规则生成账号名（不查重、不落库，纯计算）。
func (p *Pool) BuildNames(s NameSpec) ([]string, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	start := s.Start
	if s.AutoStart {
		if n := p.NextSeq(s.Prefix, s.Suffix) + 1; n > start {
			start = n
		}
	}
	if start <= 0 {
		start = 1
	}
	width := s.Pad
	if width == 0 { // 自动：按最大序号的位数补零
		width = len(strconv.Itoa(start + s.Count - 1))
	}
	out := make([]string, 0, s.Count)
	for i := 0; i < s.Count; i++ {
		out = append(out, fmt.Sprintf("%s%0*d%s", s.Prefix, width, start+i, s.Suffix))
	}
	return out, nil
}

// NamePreview 面板预览用：首尾账号 + 个数（参数不合法时返回提示）。
func (s NameSpec) Preview(names []string) map[string]any {
	out := map[string]any{"count": len(names), "prefix": s.Prefix, "suffix": s.Suffix}
	if len(names) > 0 {
		out["first"], out["last"] = names[0], names[len(names)-1]
	}
	return out
}
