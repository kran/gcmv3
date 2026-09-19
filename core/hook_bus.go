// 通用 hook 总线（WP 字符串模型 × Go 类型安全的交点）:
//
//	DefineHook 声明名字与签名（proto 必须 func(...) error）
//	AddHook    注册 handler, 运行时校验签名可赋值（注册即报错, 不留到触发）
//	Fire       按优先级+注册序调用; panic 中断剩余; 业务 err 继续（收集语义）, 返回第一个 err
//
// 变换（filter）没有特殊机制 —— handler 传指针就地修改, C 语言的地址语义。
//
// 每套装配一个 HookBus 实例（一个站点一个）—— 隔离是结构性的, 不靠命名空间前缀。
//
// **事件名不在这里定义**: 常量落在各自的语义文件（节点写路径在 node 那份文件里）。
// 总线本身不知道任何 CMS 语义, 只是"名字 → 签名 → handler 列表"。
package core

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
)

var errorType = reflect.TypeFor[error]()

type entry struct {
	priority int
	seq      int
	fn       any
}

// hook 一个事件: 名字 + 签名（proto）+ 已排序的 handler 列表。
type hook struct {
	name     string
	proto    reflect.Type
	handlers []entry
}

// HookBus 事件总线（名字 → 签名 → handler）。
type HookBus struct {
	mu    sync.RWMutex
	hooks map[string]*hook
	seq   int
}

func NewHookBus() *HookBus {
	return &HookBus{hooks: map[string]*hook{}}
}

// Defined 事件是否已声明（DefineHook 过）。与 Has 区分:
// 已声明但没有 handler 的事件仍然 Defined。
func (b *HookBus) Defined(name string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.hooks[name]
	return ok
}

// Has 事件是否注册了 handler。按名字动态拼出来的事件（"这个类型注册了规则吗"）
// 用它判断 —— 拼错与"注册了但恰好没 handler"要能分开。
func (b *HookBus) Has(name string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	h, ok := b.hooks[name]
	return ok && len(h.handlers) > 0
}

// Define 批量声明 hook（语义同 DefineHook）: 一个 map, key=事件名, value=proto 函数。
// 单条失败即报错停止。
func (b *HookBus) Define(specs map[string]any) error {
	for name, proto := range specs {
		if err := b.DefineHook(name, proto); err != nil {
			return err
		}
	}
	return nil
}

// DefineHook 声明 hook 名与签名。proto 必须是一个函数且恰好返回一个
// error（参数不限 — 变换靠指针就地修改）。重复定义报错。
func (b *HookBus) DefineHook(name string, proto any) error {
	t := reflect.TypeOf(proto)
	if t == nil || t.Kind() != reflect.Func {
		return fmt.Errorf("hook: %q proto must be a function", name)
	}
	if t.NumOut() != 1 || !t.Out(0).AssignableTo(errorType) {
		return fmt.Errorf("hook: %q proto must return exactly error, got %v", name, t)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.hooks[name]; ok {
		return fmt.Errorf("hook: %q already defined", name)
	}
	b.hooks[name] = &hook{name: name, proto: t}
	return nil
}

// AddHook 注册 handler。fn 的签名必须可赋值给 DefineHook 声明的 proto
// （注册即校验, 拼错名字/写错签名当场报错）。priority 缺省 0, 同优先级按
// 注册序稳定执行。
func (b *HookBus) AddHook(name string, fn any, priority ...int) error {
	b.mu.RLock()
	h, ok := b.hooks[name]
	b.mu.RUnlock()
	if !ok {
		return fmt.Errorf("hook: %q not defined (DefineHook first)", name)
	}
	ft := reflect.TypeOf(fn)
	if ft == nil || ft.Kind() != reflect.Func || !ft.AssignableTo(h.proto) {
		return fmt.Errorf("hook: %q expects %v, got %v", name, h.proto, ft)
	}
	p := 0
	if len(priority) > 0 {
		p = priority[0]
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// 注册时有序插入（priority 升序; 同优先级保持注册序稳定）—
	// Fire 直接遍历, 不在触发路径排序（高频事件零排序开销）。
	e := entry{priority: p, seq: b.seq, fn: fn}
	i := sort.Search(len(h.handlers), func(i int) bool {
		return h.handlers[i].priority > p
	})
	h.handlers = append(h.handlers, entry{})
	copy(h.handlers[i+1:], h.handlers[i:])
	h.handlers[i] = e
	b.seq++
	return nil
}

// Fire 触发 hook: 实参与 proto 校验（防反射 panic）→ 按优先级+注册序调用。
// panic 天然打断循环（剩余 handler 不执行）→ defer recover 包装 err 返回;
// 业务 err 不 panic — 循环继续, 返回第一个业务 err（fail-closed）。
func (b *HookBus) Fire(name string, args ...any) (err error) {
	b.mu.RLock()
	h, ok := b.hooks[name]
	b.mu.RUnlock()
	if !ok {
		return fmt.Errorf("hook: %q not defined", name)
	}
	callArgs, err := checkHookArgs(h.proto, args)
	if err != nil {
		return fmt.Errorf("hook: %q: %w", name, err)
	}

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("hook %q panicked: %v", name, r)
		}
	}()

	// 顺序在 AddHook 已排好（priority + 注册序稳定）, 触发路径零排序。
	// 拷一份再跑: handler 里 AddHook 不会撞上正在遍历的切片。
	b.mu.RLock()
	handlers := append([]entry(nil), h.handlers...)
	b.mu.RUnlock()
	var firstErr error
	for _, hd := range handlers {
		rets := reflect.ValueOf(hd.fn).Call(callArgs)
		if e, ok := rets[0].Interface().(error); ok && e != nil && firstErr == nil {
			firstErr = e
		}
	}
	return firstErr
}

// checkHookArgs 实参与 proto 参数逐项可赋值校验; nil 实参按对应参数类型的
// 零值处理（指针/接口/map/切片/chan/func 可为 nil）。
func checkHookArgs(proto reflect.Type, args []any) ([]reflect.Value, error) {
	if len(args) != proto.NumIn() {
		return nil, fmt.Errorf("expects %d args, got %d", proto.NumIn(), len(args))
	}
	out := make([]reflect.Value, len(args))
	for i, arg := range args {
		pt := proto.In(i)
		v := reflect.ValueOf(arg)
		if !v.IsValid() { // nil
			switch pt.Kind() {
			case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
				v = reflect.Zero(pt)
			default:
				return nil, fmt.Errorf("arg %d: nil not assignable to %v", i, pt)
			}
		} else if !v.Type().AssignableTo(pt) {
			return nil, fmt.Errorf("arg %d: %v not assignable to %v", i, v.Type(), pt)
		}
		out[i] = v
	}
	return out, nil
}
