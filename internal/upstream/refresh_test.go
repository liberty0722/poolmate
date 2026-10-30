package upstream

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"poolmate/internal/auth"
)

// rtFunc 把出站请求直接交给本地 handler，不走网络。
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testClient(h http.HandlerFunc) *Client {
	return &Client{HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		h(rec, r)
		return rec.Result(), nil
	})}}
}

func testAuth() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at0", RefreshToken: "rt0", ExpiresAt: time.Now().Add(time.Minute).Unix(), Realm: "cn"}
}

// 并发刷新同一账号只真正请求一次上游，其余调用复用结果。
func TestRefreshSerializedPerAccount(t *testing.T) {
	var calls atomic.Int32
	c := testClient(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("X-Refresh-Token") != "rt0" {
			w.WriteHeader(401) // 拿已轮换作废的 refresh token 请求
			return
		}
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintf(w, `{"code":0,"data":{"accessToken":"at%d","refreshToken":"rt%d","expiresIn":604800}}`, n, n)
	})
	a := testAuth()
	var saves atomic.Int32
	c.SaveAuth = func(*auth.Auth) { saves.Add(1) }
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- c.RefreshToken(a) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发刷新出现失败: %v", err)
		}
	}
	if calls.Load() != 1 || saves.Load() != 1 {
		t.Fatalf("应只请求/落盘 1 次，实际请求 %d 次、落盘 %d 次", calls.Load(), saves.Load())
	}
	if last, msg := a.RefreshStatus(); last.IsZero() || msg != "" {
		t.Fatalf("刷新状态未记录: last=%v err=%q", last, msg)
	}
}

// 网络/5xx 是临时故障，不能被判为凭证失效。
func TestRefreshTransientNotAuth(t *testing.T) {
	c := testClient(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) })
	err := c.RefreshToken(testAuth())
	if err == nil || KindOf(err) == ErrAuth {
		t.Fatalf("5xx 不应判为 ErrAuth: kind=%v err=%v", KindOf(err), err)
	}
	c = &Client{HTTP: &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial timeout")
	})}}
	if err := c.RefreshToken(testAuth()); KindOf(err) != ErrNetwork {
		t.Fatalf("网络错误应为 ErrNetwork，实际 %v", KindOf(err))
	}
}

// 上游业务拒绝（refresh token 失效）判为 ErrAuth，并记录失败原因。
func TestRefreshRejectedIsAuth(t *testing.T) {
	c := testClient(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":10001,"msg":"refresh token invalid"}`)
	})
	a := testAuth()
	if err := c.RefreshToken(a); KindOf(err) != ErrAuth {
		t.Fatalf("业务拒绝应为 ErrAuth，实际 %v (%v)", KindOf(err), err)
	}
	if _, msg := a.RefreshStatus(); msg == "" {
		t.Fatal("失败原因未记录")
	}
}

// EnsureFresh：未到阈值不刷新；1 小时内已成功刷新过不重复刷新。
func TestEnsureFresh(t *testing.T) {
	var calls atomic.Int32
	c := testClient(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"code":0,"data":{"accessToken":"atN","refreshToken":"rtN","expiresIn":43200}}`) // 只给 12 小时
	})
	a := testAuth()
	a.ExpiresAt = time.Now().Add(3 * 24 * time.Hour).Unix()
	if err := c.EnsureFresh(a, 24*time.Hour); err != nil || calls.Load() != 0 {
		t.Fatalf("剩余 3 天不应刷新: calls=%d err=%v", calls.Load(), err)
	}
	a.ExpiresAt = time.Now().Add(time.Hour).Unix()
	_ = c.EnsureFresh(a, 24*time.Hour)
	_ = c.EnsureFresh(a, 24*time.Hour) // 新 token 仍不足 24h，但 1 小时内刚刷过
	if calls.Load() != 1 {
		t.Fatalf("1 小时内应只刷新 1 次，实际 %d", calls.Load())
	}
}
