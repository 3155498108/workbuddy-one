package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestVersionNums(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"1.0.2", []int{1, 0, 2}},
		{"v1.2.10", []int{1, 2, 10}},
		{"v2", []int{2}},
		{"1.0.2-beta", []int{1, 0, 2}},
		{"", nil},
		{"abc", nil},
	}
	for _, c := range cases {
		got := versionNums(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("versionNums(%q)=%v want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("versionNums(%q)=%v want %v", c.in, got, c.want)
			}
		}
	}
}

// TestVersionLess 版本比较的关键点：段数不同、两位数段、v 前缀、带后缀。
// 写错这里会让「已是最新」和「有新版本」互串，用户要么被反复提示，
// 要么永远收不到提示。
func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.0.1", "1.0.2", true},
		{"1.0.2", "1.0.1", false},
		{"1.0.2", "1.0.2", false},
		{"1.0.9", "1.0.10", true}, // 按数字比，不是按字符串比
		{"v1.0.9", "v1.0.10", true},
		{"1.0", "1.0.0", false},
		{"1.0.0", "1.0", false},
		{"1.2", "1.10", true},
		{"9.9.9", "10.0.0", true},
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Fatalf("versionLess(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestPickAsset 资产列表里混着源码包和其他文件时，必须只挑 Windows 包，
// 并且挑版本最高的那个——挑错了会把源码包当程序下载下来。
func TestPickAsset(t *testing.T) {
	rel := &ghRelease{
		Tag: "v1.0.3",
		Assets: []ghAsset{
			{Name: "Source code (zip)", Size: 10, URL: "u0"},
			{Name: "WorkBuddyOne-v1.0.2-win64.zip", Size: 200, URL: "u1"},
			{Name: "WorkBuddyOne-v1.0.10-win64.zip", Size: 300, URL: "u2"},
			{Name: "SHA256-1.0.10.txt", Size: 80, URL: "u3"},
			{Name: "WorkBuddyOne-v1.0.3-win64.zip.sig", Size: 5, URL: "u4"},
		},
	}
	name, url, size := pickAsset(rel)
	if name != "WorkBuddyOne-v1.0.10-win64.zip" || url != "u2" || size != 300 {
		t.Fatalf("pickAsset=%q,%q,%d", name, url, size)
	}
}

func TestPickAssetEmpty(t *testing.T) {
	rel := &ghRelease{Tag: "v1.0.3", Assets: []ghAsset{{Name: "source.zip", URL: "u"}}}
	if name, _, _ := pickAsset(rel); name != "" {
		t.Fatalf("没有 Windows 包时应返回空，得到 %q", name)
	}
}

// TestShellHTMLPlaceholders 界面的 {{...}} 占位符必须全部被替换掉，
// 否则用户会在页面上直接看到 "{{VERSION}}"。
func TestShellHTMLPlaceholders(t *testing.T) {
	html := string(shellHTML())
	if strings.Contains(html, "{{API_KEY}}") {
		t.Fatal("{{API_KEY}} 没有被替换")
	}
	if strings.Contains(html, "{{VERSION}}") {
		t.Fatal("{{VERSION}} 没有被替换")
	}
	if !strings.Contains(html, appVersion) {
		t.Fatalf("页面里找不到当前版本号 %s", appVersion)
	}
	// 更新入口必须存在，否则用户没地方点。
	if !strings.Contains(html, "btnUpd") {
		t.Fatal("界面缺少更新按钮")
	}
	// V1.0.7 起更新全自动，「自动安装」勾选已移除。它要是又冒出来，
	// 说明有人把可关闭的半自动逻辑加回来了。
	if strings.Contains(html, "chkAutoApply") || strings.Contains(html, "updAutoBox") {
		t.Fatal("界面上不应再有「自动安装」开关")
	}
	if strings.Contains(html, "/update/auto_apply") {
		t.Fatal("界面不该再调用已删除的 auto_apply 接口")
	}
}

func TestTagFromReleaseURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r/releases/tag/v1.0.6":     "v1.0.6",
		"https://github.com/o/r/releases/tag/v1.0.6?x=1": "v1.0.6",
		`<a href="/o/r/releases/tag/v2.3.4">latest</a>`:  "v2.3.4",
		"https://github.com/o/r/releases":                "",
		"https://github.com/o/r/releases/tag/latest":     "",
		"": "",
	}
	for in, want := range cases {
		if got := tagFromReleaseURL(in); got != want {
			t.Errorf("tagFromReleaseURL(%q)=%q want %q", in, got, want)
		}
	}
}

// TestReleaseForTag 不查 API 时要能自己拼出正确的资产名和地址。
func TestReleaseForTag(t *testing.T) {
	rel := releaseForTag("v1.2.3")
	if rel.Tag != "v1.2.3" {
		t.Errorf("Tag=%q", rel.Tag)
	}
	name, url, _ := pickAsset(rel)
	if name != "WorkBuddyOne-v1.2.3-win64.zip" {
		t.Errorf("asset 名拼错：%q", name)
	}
	if !strings.HasSuffix(url, "/releases/download/v1.2.3/WorkBuddyOne-v1.2.3-win64.zip") {
		t.Errorf("下载地址拼错：%q", url)
	}
}

// TestReleaseFromRedirect 零配额路径：读 302 的 Location 拿 tag，不解析 API JSON。
func TestReleaseFromRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://github.com/o/r/releases/tag/v1.2.3", http.StatusFound)
	}))
	defer srv.Close()

	rel, err := releaseFromRedirectURL(srv.URL)
	if err != nil {
		t.Fatalf("应能从 302 拿到版本：%v", err)
	}
	if rel.Tag != "v1.2.3" {
		t.Errorf("Tag=%q want v1.2.3", rel.Tag)
	}
}

// TestReleaseFromRedirectProxyStrips302 有些代理会把 302 拆掉直接回 200 页面，
// 这时要从正文里再找一次，不能直接判失败。
func TestReleaseFromRedirectProxyStrips302(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><a href="/o/r/releases/tag/v1.4.0">v1.4.0</a></html>`))
	}))
	defer srv.Close()

	rel, err := releaseFromRedirectURL(srv.URL)
	if err != nil {
		t.Fatalf("200 页面里也应能抠出版本号：%v", err)
	}
	if rel.Tag != "v1.4.0" {
		t.Errorf("Tag=%q want v1.4.0", rel.Tag)
	}
}

// TestRateLimitMessage API 兜底撞上 403 时，报错要说人话——
// 别把 GitHub 那串英文原文糊给用户。
func TestRateLimitMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"API rate limit exceeded for 1.2.3.4."}`))
	}))
	defer srv.Close()

	_, err := fetchReleaseAPI(srv.URL)
	if err == nil {
		t.Fatal("403 应当报错")
	}
	msg := err.Error()
	if strings.Contains(msg, "rate limit exceeded") {
		t.Errorf("不该把英文原文直接抛给用户：%s", msg)
	}
	if !strings.Contains(msg, "限流") || !strings.Contains(msg, "update_token") {
		t.Errorf("报错应说明是限流并给出解法，实际：%s", msg)
	}
}

// TestReleaseFromRedirectReal 真打 github.com 验证零配额路径。
// 设 WBONE_REAL_TAG=1 才跑（要联网）。这条路径不该消耗 API 配额，
// 也就不会 403 rate limit——用户报的正是这个。
func TestReleaseFromRedirectReal(t *testing.T) {
	if os.Getenv("WBONE_REAL_TAG") != "1" {
		t.Skip("未设置 WBONE_REAL_TAG")
	}
	rel, err := releaseFromRedirect()
	if err != nil {
		t.Fatalf("零配额查版本失败：%v", err)
	}
	name, url, _ := pickAsset(rel)
	t.Logf("tag=%s asset=%s", rel.Tag, name)
	if rel.Tag == "" || name == "" {
		t.Fatal("没拿到版本或资产名")
	}
	// 拼出来的下载地址必须是真实存在的（HEAD 不跟随跳转，302 即表示有货）。
	req, _ := http.NewRequest(http.MethodHead, url, nil)
	c := &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("探测下载地址失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusOK {
		t.Fatalf("下载地址返回 %d，说明资产名拼错了", resp.StatusCode)
	}
	t.Logf("下载地址可用（HTTP %d）", resp.StatusCode)
}
