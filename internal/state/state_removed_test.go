// removed 名单基础单测（2026-10-12 号池轮换配套）：UnmarkRemoved 的返回值与幂等语义
// （/api/robots/unremove 与 rotation 到龄回池都依赖"返回原本是否在名单里"）。
package state

import "testing"

func TestUnmarkRemovedIdempotent(t *testing.T) {
	s := New()
	if s.UnmarkRemoved("a@xy3.com") {
		t.Fatal("不在名单里应返回 false")
	}
	if s.IsRemoved("a@xy3.com") {
		t.Fatal("不该凭空标记")
	}
	s.MarkRemoved("a@xy3.com")
	if !s.IsRemoved("a@xy3.com") {
		t.Fatal("MarkRemoved 后应在名单里")
	}
	if !s.UnmarkRemoved("a@xy3.com") {
		t.Fatal("在名单里应返回 true")
	}
	if s.IsRemoved("a@xy3.com") {
		t.Fatal("解除后不该还在名单里")
	}
	// 第二次解除：幂等 false，不影响其它号
	s.MarkRemoved("b@xy3.com")
	if s.UnmarkRemoved("a@xy3.com") {
		t.Fatal("重复解除应返回 false（幂等）")
	}
	if !s.IsRemoved("b@xy3.com") {
		t.Fatal("解除 a 不应影响 b")
	}
}
