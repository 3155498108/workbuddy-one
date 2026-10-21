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

// TestOwnInstanceTrueWhenOurs 自己的 /accounts/api/state 应答时必须认出来。
func TestOwnInstanceTrueWhenOurs(t *testing.T) {
	serveOn(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/api/state" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"running":true,"accounts":[{"uid":"u1"}]}`))
	})
	if !ownInstance() {
		t.Fatal("ownInstance 应返回 true")
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

// TestOwnInstanceSplitsBodyAcrossReads 关键回归：响应体分两段到达时仍要认出自己。
//
// 早先的实现只做一次 resp.Body.Read(buf[:256])，拿到多少算多少。真实响应
// （含 current/accounts/auth_dir 等）有 800+ 字节，而 "accounts" 排在 current
// 之后、偏移两百多——一次 Read 恰好只返回了前一段时，就会把「是我自己」误判成
// 「不是我」，waitReady 空转 20 秒后打印「网关启动超时」自己退出。
// 这里先写一段、Flush、再写剩下的，确保第一次 Read 一定读不满。
func TestOwnInstanceSplitsBodyAcrossReads(t *testing.T) {
	serveOn(t, func(w http.ResponseWriter, r *http.Request) {
		defer func() { _, _ = w.Write([]byte("x")) }()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"current":[`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"uid":"u1"}],"running":true,"accounts":[{"uid":"u1"}]}`))
	})
	if !ownInstance() {
		t.Fatal("响应体分段到达时应仍能认出本实例")
	}
}
