// 机器人进程管理：按「部署目录」清理同目录残留（绝不按镜像名——同机可能跑着
// py 中控/测试中控的机器人，镜像名一样）。
package process_test

import (
	"testing"

	"zyctrlcenter/internal/services/process"
)

func TestParsePIDs(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"15880\r\n39892\r\n", []int{15880, 39892}}, // 逐行输出（CRLF）
		{"15880\n\n39892\n", []int{15880, 39892}},   // 夹杂空行
		{"  42  \n", []int{42}},                     // 前后空白
		{"", nil},                                   // 空输出
		{"not-a-pid\nabc\n", nil},                   // 非数字行忽略
		{"0\n-5\n", nil},                            // 非法 PID 忽略
	}
	for _, c := range cases {
		got := process.ParsePIDs(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("ParsePIDs(%q) = %v，期望 %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("ParsePIDs(%q) = %v，期望 %v", c.in, got, c.want)
			}
		}
	}
}
