package main

import (
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// serveOn 在 127.0.0.1 的随机端口上起一个服务，并把 appPort 指过去，
// 让 ownInstance() 打过来的请求落在这个测试服务上。
func serveOn(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	old := appPort
	appPort = strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		appPort = old
	})
}

// TestOwnInstanceTrueWhenOurs 自己的 ping 应答时必须认出来。
func TestOwnInstanceTrueWhenOurs(t *testing.T) {
	serveOn(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/api/ping" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"app":"wbone","ok":true,"version":"1.0.2"}`))
	})
	if !ownInstance() {
		t.Fatal("ownInstance 应返回 true")
	}
}

// TestOwnInstanceUsesPingNotState 自检绝不能打在 /accounts/api/state 上——
// 那个接口要扫对话数据库，客户端锁着库时会卡到超时，自检就会误判。
func TestOwnInstanceUsesPingNotState(t *testing.T) {
	var hitPing, hitState bool
	serveOn(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/accounts/api/ping":
			hitPing = true
			_, _ = w.Write([]byte(`{"app":"wbone","ok":true}`))
		case "/accounts/api/state":
			hitState = true
			http.Error(w, "slow", http.StatusGatewayTimeout)
		default:
			http.NotFound(w, r)
		}
	})
	ownInstance()
	if !hitPing {
		t.Fatal("自检应该请求 /accounts/api/ping")
	}
	if hitState {
		t.Fatal("自检不应该请求 /accounts/api/state（要扫数据库，会慢到超时）")
	}
}

// TestOwnInstanceFalseOn404 端口上是别的程序（404）时不能认成自己，
// 否则单实例逻辑会去唤一个不存在的窗口。
func TestOwnInstanceFalseOn404(t *testing.T) {
	serveOn(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	if ownInstance() {
		t.Fatal("404 不应被认成本实例")
	}
}

// TestOwnInstanceFalseOnForeignJSON 别的程序碰巧在 7863 上回了 JSON，也不能认。
func TestOwnInstanceFalseOnForeignJSON(t *testing.T) {
	serveOn(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"accounts":[]}`))
	})
	if ownInstance() {
		t.Fatal("没有 app=wbone 标记就不该认成本实例")
	}
}

// TestOwnInstanceSplitsBodyAcrossReads 关键回归：响应体分两段到达时仍要认出自己。
//
// 早先的实现只做一次 resp.Body.Read(buf[:256])，拿到多少算多少。真实响应
// 有几百字节，标记词若落在第一段之外，就会把「是我自己」误判成「不是我」，
// 于是 waitReady 空转、进程自己退出，而服务其实在正常监听。
// 这里先写一段、Flush、再写剩下的，确保第一次 Read 一定读不满。
func TestOwnInstanceSplitsBodyAcrossReads(t *testing.T) {
	serveOn(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"padding":"`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`0123456789","app":"wbone","version":"1.0.2"}`))
	})
	if !ownInstance() {
		t.Fatal("响应体分段到达时应仍能认出本实例")
	}
}
