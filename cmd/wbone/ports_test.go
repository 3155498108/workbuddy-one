package main

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
)

// freePort 拿一个当前确定空闲的端口号（Listen :0 让系统分配后立刻放掉）。
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return strconv.Itoa(p)
}

// TestEnsurePortKeepsFree 端口空闲时不该动它，也不该留 env 副作用。
func TestEnsurePortKeepsFree(t *testing.T) {
	t.Setenv("WB2A_LISTEN", "")
	port := freePort(t)
	got, note := ensurePort("127.0.0.1:" + port)
	if got != port || note != "" {
		t.Fatalf("空闲端口被改动了：got=%s note=%q", got, note)
	}
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		t.Fatalf("不该设置 WB2A_LISTEN，实际 %q", v)
	}
}

// TestEnsurePortMovesWhenBusy 端口被占时顺延，并把新地址写进 WB2A_LISTEN。
// 这对应 issue #1 的「后台服务意外退出」：以前这里直接弹框退出，程序再也起不来。
func TestEnsurePortMovesWhenBusy(t *testing.T) {
	t.Setenv("WB2A_LISTEN", "")

	// 找一个 base，让 base+1..+9 里至少有一个空闲
	base := 39900
	for try := 0; try < 40; try++ {
		b := base + try*10
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(b))
		if err != nil {
			continue
		}
		anyFree := false
		for i := 1; i <= 9; i++ {
			if l2, e := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(b+i)); e == nil {
				_ = l2.Close()
				anyFree = true
				break
			}
		}
		if !anyFree {
			_ = ln.Close()
			continue
		}
		defer ln.Close()

		got, note := ensurePort("127.0.0.1:" + strconv.Itoa(b))
		if got == strconv.Itoa(b) {
			t.Fatalf("端口被占却仍返回原端口 %s", got)
		}
		if note == "" {
			t.Fatal("换端口时必须给出说明，否则用户看不到 API 地址变了")
		}
		if !strings.Contains(note, got) {
			t.Fatalf("说明里应包含新端口 %s，实际 %q", got, note)
		}
		if v := os.Getenv("WB2A_LISTEN"); v != "127.0.0.1:"+got {
			t.Fatalf("WB2A_LISTEN 应为 127.0.0.1:%s，实际 %q", got, v)
		}
		if !portFree(got) {
			t.Fatalf("换到的 %s 其实不空闲", got)
		}
		return
	}
	t.Skip("找不到可用的测试端口段")
}

// TestEnsurePortKeepsHost 换端口只换端口号，不能把监听地址从所有网卡悄悄收回回环。
// 用户在 config.json 里写了 `:7863` 是有意的（局域网里的别的机器要调 API），
// 我们顺延时若改成 127.0.0.1，那些调用方会莫名其妙连不上。
func TestEnsurePortKeepsHost(t *testing.T) {
	t.Setenv("WB2A_LISTEN", "")
	for try := 0; try < 40; try++ {
		b := 41100 + try*10
		ln, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(b))
		if err != nil {
			continue
		}
		anyFree := false
		for i := 1; i <= 9; i++ {
			if l2, e := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(b+i)); e == nil {
				_ = l2.Close()
				anyFree = true
				break
			}
		}
		if !anyFree {
			_ = ln.Close()
			continue
		}
		defer ln.Close()

		got, note := ensurePort("0.0.0.0:" + strconv.Itoa(b))
		if note == "" || got == strconv.Itoa(b) {
			t.Fatalf("应换端口并给出说明：got=%s note=%q", got, note)
		}
		if v := os.Getenv("WB2A_LISTEN"); v != "0.0.0.0:"+got {
			t.Fatalf("换端口后应仍监听所有网卡，实际 %q", v)
		}
		return
	}
	t.Skip("找不到可用的测试端口段")
}

// TestExplainGwErr 网关错误要翻成人话，不能把英文原文直接甩给用户。
func TestExplainGwErr(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("http: listen tcp 127.0.0.1:7863: bind: Only one usage of each socket address"), "端口被占用"},
		{errors.New("load config: invalid character"), "配置文件"},
		{errors.New("open data/state.json: Access is denied."), "写权限"},
	}
	for _, c := range cases {
		got := explainGwErr(c.err)
		if !strings.Contains(got, c.want) {
			t.Fatalf("%v 的提示应含「%s」，实际 %q", c.err, c.want, got)
		}
		// 原文要保留：用户拿去搜也好，贴给开发者也好，都比只看中文有用
		if !strings.Contains(got, c.err.Error()) {
			t.Fatalf("%v 的提示丢了原文", c.err)
		}
	}
	// 认不出来的错误原样返回，不要瞎猜
	if got := explainGwErr(errors.New("weird")); got != "weird" {
		t.Fatalf("未知错误应原样返回，实际 %q", got)
	}
}
