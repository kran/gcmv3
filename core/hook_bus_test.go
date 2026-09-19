package core

import (
	"errors"
	"strings"
	"testing"
)

func TestDefineHook(t *testing.T) {
	b := NewHookBus()
	if err := b.DefineHook("a", 42); err == nil {
		t.Fatal("non-func proto must be rejected")
	}
	if err := b.DefineHook("a", func(x int) int { return x }); err == nil {
		t.Fatal("proto without error return must be rejected")
	}
	if err := b.DefineHook("a", func(x int) error { return nil }); err != nil {
		t.Fatalf("valid proto: %v", err)
	}
	if err := b.DefineHook("a", func(x int) error { return nil }); err == nil {
		t.Fatal("re-define must be rejected")
	}
}

// Defined 与 Has 分开: 声明过但没注册 handler, 仍然 Defined。
func TestDefinedVsHas(t *testing.T) {
	b := NewHookBus()
	if b.Defined("e") || b.Has("e") {
		t.Fatal("unknown event")
	}
	if err := b.DefineHook("e", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !b.Defined("e") {
		t.Fatal("declared event must be Defined")
	}
	if b.Has("e") {
		t.Fatal("declared without handler must not Has")
	}
	if err := b.AddHook("e", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !b.Has("e") {
		t.Fatal("event with handler must Has")
	}
}

// 顺序: priority 升序; 同优先级按注册序稳定（AddHook 时已排好）。
func TestOrdering(t *testing.T) {
	b := NewHookBus()
	b.DefineHook("e", func() error { return nil })
	var order []string
	b.AddHook("e", func() error { order = append(order, "p10"); return nil }, 10)
	b.AddHook("e", func() error { order = append(order, "p1a"); return nil }, 1)
	b.AddHook("e", func() error { order = append(order, "p0"); return nil }, 0)
	b.AddHook("e", func() error { order = append(order, "p1b"); return nil }, 1)
	if err := b.Fire("e"); err != nil {
		t.Fatal(err)
	}
	want := "p0,p1a,p1b,p10"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}

// 业务 err 继续执行剩余 hook（收集语义）; Fire 返回第一个 err。
func TestErrContinue(t *testing.T) {
	b := NewHookBus()
	b.DefineHook("e", func() error { return nil })
	var called []string
	b.AddHook("e", func() error { called = append(called, "a"); return nil })
	b.AddHook("e", func() error { called = append(called, "b"); return errors.New("stop") })
	b.AddHook("e", func() error { called = append(called, "c"); return nil })
	err := b.Fire("e")
	if err == nil || !strings.Contains(err.Error(), "stop") {
		t.Fatalf("must return first err, got %v", err)
	}
	if len(called) != 3 {
		t.Fatalf("err must not stop others, got %v", called)
	}
}

// 签名校验在注册时（拼错名字/写错签名当场报错）。
func TestAddValidation(t *testing.T) {
	b := NewHookBus()
	b.DefineHook("e", func(x int) error { return nil })
	if err := b.AddHook("ghost", func() error { return nil }); err == nil {
		t.Fatal("unknown hook must fail")
	}
	if err := b.AddHook("e", func(s string) error { return nil }); err == nil {
		t.Fatal("wrong signature must fail")
	}
	if err := b.AddHook("e", func(x int) error { return nil }); err != nil {
		t.Fatalf("valid add: %v", err)
	}
}

// hook panic 被 recover —— 返回包装 err, 不继续剩余 handler。
func TestPanicRecover(t *testing.T) {
	b := NewHookBus()
	b.DefineHook("e", func() error { return nil })
	var called []string
	b.AddHook("e", func() error { called = append(called, "a"); return nil })
	b.AddHook("e", func() error { panic("boom") })
	b.AddHook("e", func() error { called = append(called, "c"); return nil })
	err := b.Fire("e")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("must recover panic, got %v", err)
	}
	if len(called) != 1 || called[0] != "a" {
		t.Fatalf("must stop after panic, got %v", called)
	}
}

// 变换（filter）: handler 传指针就地修改 —— 总线本身没有第二种机制。
func TestPointerMutation(t *testing.T) {
	type payload struct{ Value []string }
	b := NewHookBus()
	if err := b.DefineHook("collect", func(p *payload) error { return nil }); err != nil {
		t.Fatal(err)
	}
	b.AddHook("collect", func(p *payload) error {
		p.Value = append(p.Value, "late")
		return nil
	}, 10)
	b.AddHook("collect", func(p *payload) error {
		p.Value = append(p.Value, "early")
		return nil
	}, 1)
	in := &payload{}
	if err := b.Fire("collect", in); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(in.Value, ","); got != "early,late" {
		t.Fatalf("mutation order = %s", got)
	}
}

// 实参校验: 个数不符 / 类型不符 / nil 实参都要在触发前报错（防反射 panic）。
func TestFireArgValidation(t *testing.T) {
	b := NewHookBus()
	b.DefineHook("e", func(p *payload) error { return nil })
	if err := b.Fire("e"); err == nil {
		t.Fatal("arg count mismatch must fail")
	}
	if err := b.Fire("e", 1); err == nil {
		t.Fatal("arg type mismatch must fail")
	}
	if err := b.Fire("e", nil); err != nil {
		t.Fatalf("nil pointer arg must be accepted: %v", err)
	}
	if err := b.Fire("ghost"); err == nil {
		t.Fatal("undefined event must fail")
	}
}

type payload struct{ Value []string }
