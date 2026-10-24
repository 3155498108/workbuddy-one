// window_test.go 覆盖「窗口活着还是死了」的判定。
//
// 这套判定的存在理由：只按 cmd.Start() 拿到的那个 pid 判活，会在 Edge 首次
// 初始化新 profile 自我 relaunch 时误判为「窗口已关闭」，随手把还开着的窗口
// kill 掉——用户看到的就是「第一次启动闪退，第二次才好」。
package main

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestMain 提供一个「长命子进程」入口：Job 计数必须用真进程验证，
// 否则 Win32 结构体布局写错了也测不出来。
func TestMain(m *testing.M) {
	if os.Getenv("WBONE_JOB_HELPER") == "1" {
		time.Sleep(20 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// spawnHelper 起一个会活 20 秒的测试子进程。
func spawnHelper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=NoSuchTestRunsHere")
	cmd.Env = append(os.Environ(), "WBONE_JOB_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("起子进程失败：%v", err)
	}
	return cmd
}

// TestJobLiveness 覆盖「主进程没了之后，Job 里剩几个进程才算窗口还在」。
func TestJobLiveness(t *testing.T) {
	cases := []struct {
		name   string
		active int
		known  bool
		want   bool
	}{
		{"新主进程 + 渲染进程 → 窗口有人接管", 4, true, true},
		{"两个进程也算接管", 2, true, true},
		{"只剩一个（crashpad 收尾）→ 已经关了", 1, true, false},
		{"Job 已空 → 已经关了", 0, true, false},
		{"查不到 Job → 不能当成活着", 5, false, false},
	}
	for _, c := range cases {
		if got := jobLiveness(c.active, c.known); got != c.want {
			t.Errorf("%s：jobLiveness(%d,%v)=%v want %v", c.name, c.active, c.known, got, c.want)
		}
	}
}

// TestJobActiveCountReal 真的建 Job、放活进程进去，验证数得出来。
func TestJobActiveCountReal(t *testing.T) {
	prevJob := edgeJob
	edgeJob = 0
	defer func() {
		if edgeJob != 0 && edgeJob != prevJob {
			_ = windows.TerminateJobObject(edgeJob, 0)
			windows.CloseHandle(edgeJob)
		}
		edgeJob = prevJob
	}()

	h1 := spawnHelper(t)
	defer func() { _ = h1.Process.Kill(); _ = h1.Wait() }()
	if err := bindBrowserToJob(h1.Process.Pid); err != nil {
		t.Fatalf("绑定 Job 失败：%v", err)
	}

	var n int
	var known bool
	for i := 0; i < 40; i++ {
		if n, known = jobActiveCount(); known && n >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !known {
		t.Fatal("Job 已绑定却查不到活跃进程数：结构体布局或信息类常量不对")
	}
	if n < 1 {
		t.Fatalf("Job 里应有至少 1 个活进程，实际 %d", n)
	}

	h2 := spawnHelper(t)
	defer func() { _ = h2.Process.Kill(); _ = h2.Wait() }()
	if err := bindBrowserToJob(h2.Process.Pid); err != nil {
		t.Fatalf("绑定第二个进程失败：%v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n, known = jobActiveCount()
		if known && n > 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !jobLiveness(n, known) {
		t.Fatalf("Job 里有两个活进程时应判定窗口还在，实际 n=%d known=%v", n, known)
	}
}

// TestBrowserWindowGoneGrace 启动后的宽限期内一律不判死。
func TestBrowserWindowGoneGrace(t *testing.T) {
	prevBoot := winBootAt
	defer func() { winBootAt = prevBoot }()
	winBootAt = time.Now()
	if browserWindowGone() {
		t.Fatal("刚拉起 10 秒内不该判定窗口关闭（Edge 此时可能正在自我重启）")
	}
}

// TestBrowserWindowGoneMainAlive 主进程还活着时立刻返回 false，不做任何延时。
func TestBrowserWindowGoneMainAlive(t *testing.T) {
	prevBoot, prevBrowser, prevJob := winBootAt, browser, edgeJob
	defer func() { winBootAt, browser, edgeJob = prevBoot, prevBrowser, prevJob }()

	winBootAt = time.Now().Add(-time.Minute)
	edgeJob = 0
	browser = &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}

	start := time.Now()
	if browserWindowGone() {
		t.Fatal("主进程还活着，不该判定窗口关闭")
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("主进程活着时不该走重试延时，实际耗时 %v", el)
	}
}

// TestBrowserWindowGoneWithFreshHeartbeat 是这次修复的现场：
// 主进程 pid 已退出、Job 也查不到，但页面还在发心跳 → 窗口其实是活的。
func TestBrowserWindowGoneWithFreshHeartbeat(t *testing.T) {
	prevBoot, prevBrowser, prevJob, prevSeen := winBootAt, browser, edgeJob, lastSeen.Load()
	defer func() {
		winBootAt, browser, edgeJob = prevBoot, prevBrowser, prevJob
		lastSeen.Store(prevSeen)
	}()

	winBootAt = time.Now().Add(-time.Minute)
	browser = nil
	edgeJob = 0
	lastSeen.Store(time.Now().UnixNano())

	if browserWindowGone() {
		t.Fatal("页面 3 秒一次心跳还在刷新，不该把窗口当成关闭（这正是误杀的现场）")
	}
}

// TestBrowserWindowGoneWithoutHeartbeat 三项判据全落空，才允许退出。
func TestBrowserWindowGoneWithoutHeartbeat(t *testing.T) {
	prevBoot, prevBrowser, prevJob, prevSeen := winBootAt, browser, edgeJob, lastSeen.Load()
	defer func() {
		winBootAt, browser, edgeJob = prevBoot, prevBrowser, prevJob
		lastSeen.Store(prevSeen)
	}()

	winBootAt = time.Now().Add(-time.Minute)
	browser = nil
	edgeJob = 0
	lastSeen.Store(time.Now().Add(-5 * time.Minute).UnixNano())

	if !browserWindowGone() {
		t.Fatal("主进程没了、Job 查不到、心跳也停了，应当判定窗口已关闭")
	}
}

// TestKillBrowserWithoutJob 没绑成 Job 时不能 panic，也不能误杀别的进程。
func TestKillBrowserWithoutJob(t *testing.T) {
	prevBrowser, prevJob := browser, edgeJob
	defer func() { browser, edgeJob = prevBrowser, prevJob }()
	browser = nil
	edgeJob = 0
	killBrowser() // 什么都不该发生
}
