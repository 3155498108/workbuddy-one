package main

import (
	"strings"
	"testing"
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
}
