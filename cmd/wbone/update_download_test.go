// update_download_test.go 钉住更新链路上最容易出错的两处：
//
//  1. 下载用「停滞检测」而不是「总时长上限」。v1.0.3 起更新失败全是
//     Client.Timeout=20s 把慢速下载从中间掐断（context deadline exceeded
//     ... while reading body）。
//  2. 解包认名字要扛住 Windows 的反斜杠分隔符。path.Base 只认 `/`，
//     "dir\WorkBuddyOne.exe" 会被当成名字里带反斜杠的文件，于是包明明是好的
//     却报「安装包里没有 WorkBuddyOne.exe」。
package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeExe 造一个能过 PE 头校验和体积下限的假程序。
func fakeExe(size int) []byte {
	b := bytes.Repeat([]byte{0x90}, size)
	b[0], b[1] = 'M', 'Z'
	return b
}

// writeZip 按给定的「条目名 → 内容」写一个 zip。名字原样写入，
// 所以可以造出反斜杠分隔符这种 Windows 打包工具的真实产物。
func writeZip(t *testing.T, path string, entries map[string][]byte, order []string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, name := range order {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entries[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestZipBaseHandlesBothSeparators(t *testing.T) {
	cases := map[string]string{
		`WorkBuddyOne.exe`:                           "WorkBuddyOne.exe",
		`WorkBuddyOne-v1.0.5-win64/WorkBuddyOne.exe`: "WorkBuddyOne.exe",
		`WorkBuddyOne-v1.0.5-win64\WorkBuddyOne.exe`: "WorkBuddyOne.exe",
		`a\b\c.exe`: "c.exe",
	}
	for in, want := range cases {
		if got := zipBase(in); got != want {
			t.Errorf("zipBase(%q)=%q want %q", in, got, want)
		}
	}
}

// TestExtractExeBackslashSeparator 是这个 bug 的回归测试：
// 旧实现用 path.Base 直接比，反斜杠包一定报「安装包里没有 WorkBuddyOne.exe」。
func TestExtractExeBackslashSeparator(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "pkg.zip")
	exe := fakeExe(updateMinExeBytes + 16)
	name := `WorkBuddyOne-v1.0.5-win64\WorkBuddyOne.exe`
	writeZip(t, zp, map[string][]byte{
		name:         exe,
		`README.txt`: []byte("hi"),
	}, []string{`README.txt`, name})

	staged, err := extractExe(zp, dir)
	if err != nil {
		t.Fatalf("反斜杠分隔的包应当能解出 exe，却报错：%v", err)
	}
	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(exe) {
		t.Errorf("取出 %d 字节，want %d", len(got), len(exe))
	}
	if !bytes.Equal(got, exe) {
		t.Error("取出的内容和包里不一致")
	}
}

// TestExtractExeCaseInsensitive 文件名规则在 Windows 上不区分大小写。
func TestExtractExeCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "pkg.zip")
	writeZip(t, zp, map[string][]byte{
		"pkg/workbuddyone.EXE": fakeExe(updateMinExeBytes + 8),
	}, []string{"pkg/workbuddyone.EXE"})

	if _, err := extractExe(zp, dir); err != nil {
		t.Fatalf("大小写不同也应认，却报错：%v", err)
	}
}

// TestExtractExeFallsBackToSoleExe 包里只有一个 exe（用户自己重打包改了名）
// 就用它——不该让更新彻底卡死。
func TestExtractExeFallsBackToSoleExe(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "pkg.zip")
	writeZip(t, zp, map[string][]byte{
		"pkg/我的管家.exe": fakeExe(updateMinExeBytes + 4),
	}, []string{"pkg/我的管家.exe"})

	if _, err := extractExe(zp, dir); err != nil {
		t.Fatalf("只有一个 exe 时应兜底采用，却报错：%v", err)
	}
}

// TestExtractExeMultipleExeRefuses 多个 exe 且没有正式名：不能瞎猜。
func TestExtractExeMultipleExeRefuses(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "pkg.zip")
	writeZip(t, zp, map[string][]byte{
		"a.exe": fakeExe(updateMinExeBytes + 1),
		"b.exe": fakeExe(updateMinExeBytes + 2),
	}, []string{"a.exe", "b.exe"})

	_, err := extractExe(zp, dir)
	if err == nil {
		t.Fatal("多个 exe 时应拒绝")
	}
	if !strings.Contains(err.Error(), "多个 exe") {
		t.Errorf("报错应说明有多个 exe，实际：%v", err)
	}
}

// TestExtractExeMissingListsContents 找不到时要说明包里到底有什么，
// 否则用户和日志都无从判断。
func TestExtractExeMissingListsContents(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "pkg.zip")
	writeZip(t, zp, map[string][]byte{
		"pkg/使用说明.txt": []byte("x"),
	}, []string{"pkg/使用说明.txt"})

	_, err := extractExe(zp, dir)
	if err == nil {
		t.Fatal("没有 exe 时应报错")
	}
	if !strings.Contains(err.Error(), "使用说明.txt") {
		t.Errorf("报错应列出包内条目，实际：%v", err)
	}
}

// withShortTimeouts 把重试/停滞参数压到毫秒级，避免测试空等半分钟。
func withShortTimeouts(t *testing.T, stall time.Duration, tries int) {
	t.Helper()
	oldStall, oldTries, oldWait := updateStallTimeout, updateTries, updateRetryWait
	updateStallTimeout, updateTries, updateRetryWait = stall, tries, time.Millisecond
	t.Cleanup(func() {
		updateStallTimeout, updateTries, updateRetryWait = oldStall, oldTries, oldWait
	})
}

// TestDownloadClientHasNoTotalTimeout 下载 client 不能有总时长上限——
// 有的话就是 v1.0.3 那个 bug。
func TestDownloadClientHasNoTotalTimeout(t *testing.T) {
	c := updateDownloadClient()
	if c.Timeout != 0 {
		t.Errorf("下载 client Timeout=%v，必须为 0（总时长会掐断慢速下载）", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("下载 client 应带自定义 Transport")
	}
	if tr.ResponseHeaderTimeout != updateHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout=%v want %v", tr.ResponseHeaderTimeout, updateHeaderTimeout)
	}
}

// TestDownloadSlowButAlive 整个下载耗时远超停滞阈值，但每块之间的间隔都小于
// 阈值——这种「慢但活着」的下载必须成功。这就是修掉的那个 bug。
func TestDownloadSlowButAlive(t *testing.T) {
	withShortTimeouts(t, 400*time.Millisecond, 1)

	const chunks, chunkSize = 12, 256
	payload := bytes.Repeat([]byte("a"), chunks*chunkSize)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		for i := 0; i < chunks; i++ {
			if _, err := w.Write(payload[i*chunkSize : (i+1)*chunkSize]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(120 * time.Millisecond) // 总耗时 ~1.4s，单块间隔 120ms < 400ms
		}
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "pkg.zip")
	start := time.Now()
	if err := downloadFile(srv.URL, dst, 1<<20, int64(len(payload))); err != nil {
		t.Fatalf("慢速但持续的下载不该失败：%v", err)
	}
	if el := time.Since(start); el < time.Second {
		t.Errorf("耗时 %v，测试没真正覆盖「慢速」，检查分块间隔", el)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, payload) {
		t.Errorf("内容不一致：%d/%d 字节", len(got), len(payload))
	}
}

// TestDownloadStallDetected 数据停住不动：应在停滞阈值附近就失败，
// 而不是干等到某个总超时。
func TestDownloadStallDetected(t *testing.T) {
	withShortTimeouts(t, 300*time.Millisecond, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte(strings.Repeat("a", 10)))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // 卡住，直到客户端放弃
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "pkg.zip")
	start := time.Now()
	err := downloadFile(srv.URL, dst, 1<<20, 1000)
	el := time.Since(start)
	if err == nil {
		t.Fatal("卡住的下载应当失败")
	}
	if !strings.Contains(err.Error(), "下载卡住了") {
		t.Errorf("报错应说明是停滞，实际：%v", err)
	}
	if el > 3*time.Second {
		t.Errorf("停滞检测用了 %v，太慢（阈值 300ms）", el)
	}
}

// TestDownloadResumesAfterDrop 下到一半断线：第二次应带 Range 从断点续传，
// 而不是整包重来。
func TestDownloadResumesAfterDrop(t *testing.T) {
	withShortTimeouts(t, 2*time.Second, 3)

	payload := bytes.Repeat([]byte("b"), 2000)
	var ranges []string
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		rg := r.Header.Get("Range")
		ranges = append(ranges, rg)
		start := 0
		if rg != "" {
			if _, err := fmt.Sscanf(rg, "bytes=%d-", &start); err != nil {
				t.Errorf("Range 头格式不对：%q", rg)
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)-start))
		if start > 0 {
			w.WriteHeader(http.StatusPartialContent)
		}
		if call == 1 {
			// 只发一半就断开连接，模拟网络中断。
			w.Write(payload[:len(payload)/2])
			w.(http.Flusher).Flush()
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
				}
			}
			return
		}
		w.Write(payload[start:])
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "pkg.zip")
	if err := downloadFile(srv.URL, dst, 1<<20, int64(len(payload))); err != nil {
		t.Fatalf("断线后应能续传完成，却失败：%v", err)
	}
	if len(ranges) < 2 {
		t.Fatalf("只请求了 %d 次，没有重试", len(ranges))
	}
	if ranges[1] != "bytes=1000-" {
		t.Errorf("第二次 Range=%q，应从第一个断点 1000 续传", ranges[1])
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, payload) {
		t.Errorf("续传后内容不对：%d 字节", len(got))
	}
}

// TestDownloadRejectsTinyBody 拿到的不是安装包（错误页只有几百字节）时
// 要直接判死，别浪费三轮重试。
func TestDownloadRejectsTinyBody(t *testing.T) {
	withShortTimeouts(t, time.Second, 3)

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Length", "120")
		w.Write([]byte("<html>blocked</html>"))
	}))
	defer srv.Close()

	err := downloadFile(srv.URL, filepath.Join(t.TempDir(), "pkg.zip"), 1<<20, 4<<20)
	if err == nil {
		t.Fatal("明显不是安装包的内容应当失败")
	}
	if hits != 1 {
		t.Errorf("请求了 %d 次，这种失败不该重试", hits)
	}
	if !strings.Contains(err.Error(), "网络拦截") {
		t.Errorf("报错应提示可能被拦截，实际：%v", err)
	}
}

// TestDownloadRetriableClassification 钉住「值得重试」的口径。
func TestDownloadRetriableClassification(t *testing.T) {
	if downloadRetriable(fatalDownload("HTTP %d", 404)) {
		t.Error("HTTP 404 不该重试")
	}
	if !downloadRetriable(fmt.Errorf("下载中断：%w", os.ErrDeadlineExceeded)) {
		t.Error("普通网络错误应当重试")
	}
}

// TestExtractRealPackage 用真实发布包验解包。设 WBONE_TEST_ZIP 指向本地 zip 才跑。
func TestExtractRealPackage(t *testing.T) {
	zp := os.Getenv("WBONE_TEST_ZIP")
	if zp == "" {
		t.Skip("未指定 WBONE_TEST_ZIP")
	}
	dir := t.TempDir()
	staged, err := extractExe(zp, dir)
	if err != nil {
		t.Fatalf("真实安装包解不出来：%v", err)
	}
	st, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("取出 %s，%d 字节", filepath.Base(staged), st.Size())
}

// TestDownloadRealAsset 真打 GitHub（含 302 跳转到 CDN）。设 WBONE_REAL_URL 才跑，
// 用来复验「慢速网络能不能下完」——正是用户报错的那条链路。
func TestDownloadRealAsset(t *testing.T) {
	u := os.Getenv("WBONE_REAL_URL")
	if u == "" {
		t.Skip("未指定 WBONE_REAL_URL")
	}
	dst := filepath.Join(t.TempDir(), "asset.zip")
	start := time.Now()
	if err := downloadFile(u, dst, 120<<20, 0); err != nil {
		t.Fatalf("真实下载失败：%v", err)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	el := time.Since(start)
	t.Logf("下载 %d 字节，耗时 %v，平均 %d KB/s",
		st.Size(), el.Round(time.Millisecond), st.Size()/1024/int64(el.Seconds()+1))
}
