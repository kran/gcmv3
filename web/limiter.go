// 登录失败限速 —— 进程内的固定窗口计数。
//
// 为什么在应用层还要做: 部署层的 limit_req / WAF 管的是"每个 IP 的请求速率", 挡不住
// "换 IP 慢慢试**同一个账号**"（账号换不了, IP 换得了）。两层互补, 不是重复。
//
// 键是 (动作, 渠道, 方式, 标识) —— **不含 IP**, 两个原因:
//
//	① 反向代理后面 RemoteAddr 是代理地址, 键里带 IP 会把所有人的额度并成一个
//	   （几个人失败就把全站锁了）。真实 IP 只有部署层看得到 —— 那里限更准。
//	② 不带 IP 就不存在"换个 IP 又能试"的旁路（虽然换个**标识**能, 但那是另一个账号了）。
//
// 代价说清楚: 知道账号名的攻击者能让这个账号在窗口内登不上（锁的是"这个账号+这个方式",
// 不动已有会话、不影响别的账号）。窗口短（默认 5 分钟）+ 计数在成功登录时清零,
// 这个代价是划算的 —— 换来的是爆破速率从"只要能发请求"降到窗口内的 N 次。
package web

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// defaultLoginFailures 窗口内允许的失败次数; defaultLoginWindow 窗口长度。
	defaultLoginFailures = 5
	defaultLoginWindow   = 5 * time.Minute
	// maxLimiterKeys 表大小上限（防"海量不同标识"把内存撑爆; 超了就整体清一次,
	// 宁可短暂放宽也不 OOM）。
	maxLimiterKeys = 50_000
)

type limiter struct {
	mu        sync.Mutex
	entries   map[string]*attempt
	failures  int
	window    time.Duration
	lastSweep time.Time
}

type attempt struct {
	count    int
	deadline time.Time
}

func newLimiter(failures int, window time.Duration) *limiter {
	return &limiter{entries: map[string]*attempt{}, failures: failures, window: window}
}

// enabled 关闭状态（failures <= 0）—— 每个调用点都先问它, 于是"关掉限速"是零开销。
func (l *limiter) enabled() bool { return l != nil && l.failures > 0 && l.window > 0 }

// retryAfter 现在能不能试; 不能则给出还要等多久。
func (l *limiter) retryAfter(key string) (time.Duration, bool) {
	if !l.enabled() {
		return 0, true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[key]
	if !ok || !now.Before(entry.deadline) || entry.count < l.failures {
		return 0, true
	}
	return time.Until(entry.deadline), false
}

// record 记一次结果: 成功 ⇒ 清零（成功登录说明是本人, 不必继续惩罚）; 失败 ⇒ 累加。
func (l *limiter) record(key string, success bool) {
	if !l.enabled() {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if success {
		delete(l.entries, key)
		return
	}
	entry, ok := l.entries[key]
	if !ok || !now.Before(entry.deadline) {
		l.sweep(now)
		if len(l.entries) >= maxLimiterKeys {
			// 表满了: 整体清空（攻击者用海量标识灌表时, 短暂放宽比挂掉好）
			l.entries = map[string]*attempt{}
		}
		entry = &attempt{deadline: now.Add(l.window)}
		l.entries[key] = entry
	}
	entry.count++
}

// sweep 清掉过期条目。每个窗口最多扫一次（不然每次插入都 O(n)）。
func (l *limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < l.window {
		return
	}
	l.lastSweep = now
	for key, entry := range l.entries {
		if !now.Before(entry.deadline) {
			delete(l.entries, key)
		}
	}
}

// attemptKey 限速键: 动作 + 渠道 + 方式 + 标识（分隔符用不可见字符, 避免拼串撞车）。
func attemptKey(verb, realm, method, identifier string) string {
	return verb + "\x00" + realm + "\x00" + method + "\x00" + identifier
}

// LoginLimit 调登录/注册的失败限速（配置期设置）。failures <= 0 = 关闭限速。
//
// 默认 5 次失败 / 5 分钟。调小窗口/次数请看 limiter.go 顶部的取舍说明。
func (s *Site) LoginLimit(failures int, window time.Duration) {
	s.loginLimiter = newLimiter(failures, window)
}

// limited 限速检查 —— 超限时写 429 + Retry-After 并返回 true（调用方直接返回）。
func (c *CmsCtx) limited(key string) bool {
	wait, ok := c.site.loginLimiter.retryAfter(key)
	if ok {
		return false
	}
	seconds := int(wait.Seconds()) + 1
	c.SetHeader("Retry-After", strconv.Itoa(seconds))
	c.Fail(Errorf(http.StatusTooManyRequests, "尝试过于频繁, 请 %d 秒后再试", seconds))
	return true
}

// recordAttempt 记录一次尝试结果（成功清零, 失败累加）。
func (c *CmsCtx) recordAttempt(key string, success bool) {
	c.site.loginLimiter.record(key, success)
}
