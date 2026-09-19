package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kran/gcmv3/core"
	"github.com/kran/gcmv3/so"
	"github.com/kran/gcmv3/types"
)

// 连续失败到阈值 ⇒ 之后一律 429（**即使口令是对的** —— 锁定期不再算凭据）。
func TestLoginRateLimit(t *testing.T) {
	site := authSite(t)
	register := jsonDo(t, site, http.MethodPost, "/api/auth/member/register",
		`{"method":"email","identifier":"a@x.com","secret":"secret123"}`)
	if register.Code != http.StatusOK {
		t.Fatalf("注册 = %d", register.Code)
	}
	wrong := `{"method":"email","identifier":"a@x.com","secret":"wrong1234"}`
	for i := 0; i < defaultLoginFailures; i++ {
		got := jsonDo(t, site, http.MethodPost, "/api/auth/member/login", wrong)
		if got.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败 = %d %q", i+1, got.Code, got.Body.String())
		}
	}
	// 阈值用尽 ⇒ 429
	got := jsonDo(t, site, http.MethodPost, "/api/auth/member/login", wrong)
	if got.Code != http.StatusTooManyRequests {
		t.Fatalf("该 429: %d %q", got.Code, got.Body.String())
	}
	if got.Header().Get("Retry-After") == "" {
		t.Fatal("该带 Retry-After")
	}
	if !strings.Contains(got.Body.String(), `"code":"rate_limited"`) {
		t.Fatalf("code = %q", got.Body.String())
	}
	// **正确的口令也照样 429**（锁定期内不评估凭据）
	right := jsonDo(t, site, http.MethodPost, "/api/auth/member/login",
		`{"method":"email","identifier":"a@x.com","secret":"secret123"}`)
	if right.Code != http.StatusTooManyRequests {
		t.Fatalf("锁定期该 429: %d %q", right.Code, right.Body.String())
	}
	// 别的账号不受影响
	other := jsonDo(t, site, http.MethodPost, "/api/auth/member/register",
		`{"method":"email","identifier":"b@x.com","secret":"secret123"}`)
	if other.Code != http.StatusOK {
		t.Fatalf("别的账号不该被牵连: %d", other.Code)
	}
}

// 成功登录清零: 失败 4 次 + 成功 1 次 ⇒ 额度重置（不是"攒着"）。
func TestLoginRateLimitResetsOnSuccess(t *testing.T) {
	site := authSite(t)
	jsonDo(t, site, http.MethodPost, "/api/auth/member/register",
		`{"method":"email","identifier":"a@x.com","secret":"secret123"}`)
	wrong := `{"method":"email","identifier":"a@x.com","secret":"wrong1234"}`
	for i := 0; i < defaultLoginFailures-1; i++ {
		jsonDo(t, site, http.MethodPost, "/api/auth/member/login", wrong)
	}
	ok := jsonDo(t, site, http.MethodPost, "/api/auth/member/login",
		`{"method":"email","identifier":"a@x.com","secret":"secret123"}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("第 %d 次之前该还能登: %d %q", defaultLoginFailures, ok.Code, ok.Body.String())
	}
	// 清零后又能失败 4 次（若没清零, 第 1 次就 429 了）
	for i := 0; i < defaultLoginFailures; i++ {
		got := jsonDo(t, site, http.MethodPost, "/api/auth/member/login", wrong)
		if got.Code != http.StatusUnauthorized {
			t.Fatalf("清零后第 %d 次 = %d（该是 401）", i+1, got.Code)
		}
	}
}

// 窗口过期 ⇒ 自动恢复（直接测 limiter: HTTP 层每步都要跑 bcrypt, 时间窗测不准）。
func TestLimiterWindow(t *testing.T) {
	l := newLimiter(2, 40*time.Millisecond)
	key := attemptKey("login", "member", "email", "a@x.com")

	if _, ok := l.retryAfter(key); !ok {
		t.Fatal("一开始该允许")
	}
	l.record(key, false)
	l.record(key, false)
	wait, ok := l.retryAfter(key)
	if ok || wait <= 0 {
		t.Fatalf("两次失败后该拦住: ok=%v wait=%v", ok, wait)
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := l.retryAfter(key); !ok {
		t.Fatal("窗口过后该恢复")
	}
	// 成功清零
	l.record(key, false)
	l.record(key, true)
	if _, ok := l.retryAfter(key); !ok {
		t.Fatal("成功后该清零")
	}
	// 关掉限速
	off := newLimiter(0, time.Minute)
	off.record(key, false)
	off.record(key, false)
	off.record(key, false)
	if _, ok := off.retryAfter(key); !ok {
		t.Fatal("关掉后不该拦")
	}
	// 表满 ⇒ 整体清空（不 OOM）
	full := newLimiter(1, time.Minute)
	for i := 0; i < maxLimiterKeys+10; i++ {
		full.record(attemptKey("login", "m", "email", string(rune('a'+i%26))+itoa(int64(i))), false)
	}
	if len(full.entries) > maxLimiterKeys {
		t.Fatalf("表该被清空: %d", len(full.entries))
	}
}

// 关闭限速: 失败多少次都还是 401（不是 429）。
func TestLoginRateLimitDisabled(t *testing.T) {
	site := authSite(t)
	site.LoginLimit(0, 0)
	jsonDo(t, site, http.MethodPost, "/api/auth/member/register",
		`{"method":"email","identifier":"a@x.com","secret":"secret123"}`)
	wrong := `{"method":"email","identifier":"a@x.com","secret":"wrong1234"}`
	for i := 0; i < defaultLoginFailures*3; i++ {
		got := jsonDo(t, site, http.MethodPost, "/api/auth/member/login", wrong)
		if got.Code != http.StatusUnauthorized {
			t.Fatalf("关掉限速后第 %d 次 = %d", i+1, got.Code)
		}
	}
}

// 重复注册 ⇒ 409（唯一约束经 ErrDuplicate 出口），且不会因为失败把自己 500 掉。
func TestRegisterDuplicateConflict(t *testing.T) {
	site := authSite(t)
	body := `{"method":"email","identifier":"a@x.com","secret":"secret123"}`
	if got := jsonDo(t, site, http.MethodPost, "/api/auth/member/register", body); got.Code != http.StatusOK {
		t.Fatalf("首次注册 = %d", got.Code)
	}
	got := jsonDo(t, site, http.MethodPost, "/api/auth/member/register", body)
	if got.Code != http.StatusConflict {
		t.Fatalf("重复注册 = %d %q", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), `"code":"conflict"`) {
		t.Fatalf("响应 = %q", got.Body.String())
	}
}

// API 层的唯一约束: 地址撞车 ⇒ 409（不是 500）。
func TestAddressDuplicateIsConflict(t *testing.T) {
	site := newPolicySite(t)
	site.Type("article").OnRead(func(_ *CmsCtx, where *so.Where, _ *Grant) error {
		*where = so.P("true")
		return nil
	})
	site.Type("article").OnCreate(func(_ *CmsCtx, _ *core.Node, allow *Grant) error {
		allow.Add(types.RolePublic, "title", "address")
		return nil
	})
	site.Type("article").OnUpdate(func(_ *CmsCtx, _ int64, _ *core.NodePatch, allow *Grant) error {
		allow.Add(types.RolePublic, "title", "address")
		return nil
	})
	cms, _ := ctxFor(site)
	first, err := cms.Create("article", core.Fields{"title": "甲", "address": "same"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = cms.Create("article", core.Fields{"title": "乙", "address": "same"})
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict {
		t.Fatalf("地址撞车该 409: %v", err)
	}
	if conflict.Code != "duplicate" {
		t.Fatalf("code = %q", conflict.Code)
	}
	// 更新到已有地址也一样
	second, err := cms.Create("article", core.Fields{"title": "丙"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = cms.Update("article", second.ID, &core.NodePatch{
		Revision: ptrInt64(1), Fields: core.Fields{"address": "same"}})
	if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict {
		t.Fatalf("更新撞车该 409: %v", err)
	}
	_ = first
}
