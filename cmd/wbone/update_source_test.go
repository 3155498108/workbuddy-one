// update_source_test.go 钉住「国内网络能不能下下来」这条链路：
//
//  1. 查版本要能扛住 github.com 不通——并发问直连 + 各加速源，取版本号最大的那个
//     （公共反代会缓存旧页面，先到的不一定最新）。
//  2. 下载要能自动换源。以前只有一条 GitHub 直连，国内卡在
//     objects.githubusercontent.com 就彻底没救。
//  3. 换源必须重下，不能把 A 源的下半截拼到 B 源上——CRC 会对不上。
package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolatePrefs 把 baseDir 指到临时目录，避免测试读写真实的 data/prefs.json。
func isolatePrefs(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	old := exeDir
	exeDir = dir
	t.Cleanup(func() { exeDir = old })
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorURL(t *testing.T) {
	raw := "https://github.com/o/r/releases/download/v1/W.zip"
	cases := []struct{ prefix, want string }{
		{"", raw},
		{"https://ghfast.top", "https://ghfast.top/" + raw},
		{"https://ghfast.top/", "https://ghfast.top/" + raw}, // 尾部斜杠不该叠成 //
		{"  https://a.b/  ", "https://a.b/" + raw},
		{"ghfast.top", raw}, // 没有 scheme 一律当没配，宁可直连也别拼出坏地址
	}
	for _, c := range cases {
		if got := mirrorURL(c.prefix, raw); got != c.want {
			t.Errorf("mirrorURL(%q) = %q，期望 %q", c.prefix, got, c.want)
		}
	}
}

func TestDownloadSourcesOrder(t *testing.T) {
	isolatePrefs(t)
	t.Setenv("WBONE_UPDATE_MIRROR", "")
	old := updateMirrors
	updateMirrors = []string{"https://m1", "https://m2"}
	defer func() { updateMirrors = old }()

	writePrefs(prefs{UpdateMirror: "https://remembered"})
	raw := "https://github.com/o/r/releases/download/v1/W.zip"
	got := downloadSources(raw)

	// 上次成功的源排第一：国内一旦换到镜像，不该每次都在直连上白等一轮超时。
	if !strings.HasPrefix(got[0], "https://remembered/") {
		t.Fatalf("第一个应是记住的源，实际 %q", got[0])
	}
	if got[1] != raw {
		t.Fatalf("第二个应是直连，实际 %q", got[1])
	}
	if len(got) != 4 { // 记住的 + 直连 + 两个镜像
		t.Fatalf("候选源数量应为 4，实际 %d：%v", len(got), got)
	}
}

func TestDownloadSourcesEnvFirst(t *testing.T) {
	isolatePrefs(t)
	t.Setenv("WBONE_UPDATE_MIRROR", "https://env-mirror")
	raw := "https://github.com/o/r/releases/download/v1/W.zip"
	got := downloadSources(raw)
	if !strings.HasPrefix(got[0], "https://env-mirror/") {
		t.Fatalf("环境变量指定的源应排第一，实际 %q", got[0])
	}
}

func TestMirrorOf(t *testing.T) {
	raw := "https://github.com/o/r/releases/download/v1/W.zip"
	if got := mirrorOf(raw, raw); got != "" {
		t.Fatalf("直连应反解为空前缀，实际 %q", got)
	}
	if got := mirrorOf("https://ghfast.top/"+raw, raw); got != "https://ghfast.top" {
		t.Fatalf("反解前缀错误：%q", got)
	}
	if got := mirrorOf("https://other/x", raw); got != "" {
		t.Fatalf("不是这次的源应返回空，实际 %q", got)
	}
}

// TestDownloadAnySourceFallback 直连失败要自动换到加速源。
// 这是「国内下不下来」的核心兜底：单源时代这里失败就是彻底失败。
func TestDownloadAnySourceFallback(t *testing.T) {
	isolatePrefs(t)
	t.Setenv("WBONE_UPDATE_MIRROR", "")

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusInternalServerError)
	}))
	defer dead.Close()

	body := fakeExe(2 << 20)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer good.Close()

	old := updateMirrors
	updateMirrors = []string{good.URL}
	defer func() { updateMirrors = old }()

	dst := filepath.Join(t.TempDir(), "W.zip")
	used, err := downloadAnySource(dead.URL+"/x.zip", dst, 120<<20, int64(len(body)))
	if err != nil {
		t.Fatalf("直连失败后应换源成功，实际报错：%v", err)
	}
	if !strings.HasPrefix(used, good.URL) {
		t.Fatalf("应记下成功的源，实际 %q", used)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != int64(len(body)) {
		t.Fatalf("下到的文件大小 %d，应为 %d", st.Size(), len(body))
	}
}

// TestDownloadAnySourceReplacesPartialFile 换源时必须删掉上一个源的残包。
// 不删的话会把两个源的下半截拼起来，得到一个 CRC 对不上的 zip。
func TestDownloadAnySourceReplacesPartialFile(t *testing.T) {
	isolatePrefs(t)
	t.Setenv("WBONE_UPDATE_MIRROR", "")

	// 第一个源：先吐一半再掐断，模拟国内常见的「连得上但传不完」。
	partial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4194304")
		_, _ = w.Write(fakeExe(1000))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer partial.Close()

	body := fakeExe(2 << 20)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer good.Close()

	oldMirrors, oldTries, oldWait := updateMirrors, updateTries, updateRetryWait
	updateMirrors = []string{good.URL}
	updateTries, updateRetryWait = 1, 0
	defer func() {
		updateMirrors, updateTries, updateRetryWait = oldMirrors, oldTries, oldWait
	}()

	dst := filepath.Join(t.TempDir(), "W.zip")
	if _, err := downloadAnySource(partial.URL+"/x.zip", dst, 120<<20, 0); err != nil {
		t.Fatalf("应换源成功：%v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != len(body) {
		t.Fatalf("文件内容应是第二个源的完整包（%d 字节），实际 %d", len(body), len(data))
	}
}

func TestDownloadAnySourceAllFail(t *testing.T) {
	isolatePrefs(t)
	t.Setenv("WBONE_UPDATE_MIRROR", "")

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	defer dead.Close()

	old := updateMirrors
	updateMirrors = nil
	defer func() { updateMirrors = old }()

	dst := filepath.Join(t.TempDir(), "W.zip")
	_, err := downloadAnySource(dead.URL+"/x.zip", dst, 120<<20, 0)
	if err == nil {
		t.Fatal("所有源都失败时必须报错")
	}
	if !strings.Contains(err.Error(), "所有下载源都失败了") {
		t.Fatalf("报错要说明是下载源问题，实际：%v", err)
	}
	// 残包不能留着——下次会被当成续传起点。
	if _, serr := os.Stat(dst); serr == nil {
		t.Fatal("失败后应删掉残包")
	}
}

// TestBackupPathNeverCollides 备份名必须唯一。
// 固定 ".old" 会在「上一次留下的 .old 处于 delete-pending」时让 rename
// 直接 Access is denied——用户点更新后窗口关了、程序再也起不来。
func TestBackupPathNeverCollides(t *testing.T) {
	target := `C:\app\WorkBuddyOne.exe`
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		p := backupPath(target)
		if seen[p] {
			t.Fatalf("备份名重复：%s", p)
		}
		seen[p] = true
		if !strings.HasPrefix(p, target+".old-") {
			t.Fatalf("备份名形状不对：%s", p)
		}
		if p == target+".old" {
			t.Fatal("不能再用固定 .old")
		}
	}
}

func TestNewerTag(t *testing.T) {
	if !newerTag("v1.0.9", "v1.0.8") {
		t.Fatal("v1.0.9 应比 v1.0.8 新")
	}
	if newerTag("v1.0.8", "v1.0.9") {
		t.Fatal("v1.0.8 不该比 v1.0.9 新")
	}
	if newerTag("v1.0.9", "v1.0.9") {
		t.Fatal("相同版本不该算更新")
	}
}

// TestFetchReleaseFastPicksNewest 多个源都答了，要取版本号最大的。
// 公共加速源会缓存旧页面，如果取「最先返回」的，可能被顶回老版本。
func TestFetchReleaseFastPicksNewest(t *testing.T) {
	oldSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 缓存着一份旧页面，直接 200 返回
		_, _ = w.Write([]byte(`<html><a href="/3155498108/workbuddy-one/releases/tag/v1.0.7">x</a></html>`))
	}))
	defer oldSrv.Close()

	newSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/3155498108/workbuddy-one/releases/tag/v1.0.9", http.StatusFound)
	}))
	defer newSrv.Close()

	rel, err := fetchReleaseFast([]string{oldSrv.URL, newSrv.URL})
	if err != nil {
		t.Fatalf("多源查版本失败：%v", err)
	}
	if rel.Tag != "v1.0.9" {
		t.Fatalf("应取最新 tag v1.0.9，实际 %s", rel.Tag)
	}
}

func TestFetchReleaseFastAllFail(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer dead.Close()
	if _, err := fetchReleaseFast([]string{dead.URL}); err == nil {
		t.Fatal("全部源失败时必须返回错误")
	}
}
