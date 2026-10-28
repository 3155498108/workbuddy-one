package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTwoLinesMakeTwoActive 复现 issue #1 的「2 个账号都显示正在使用」。
//
// 现场：auth 目录里同时存在两条受管线路、各指向不同账号。accounts() 的 live
// 是 map[uid]，任一线路命中就把该账号标成 active；而 ui.html 对 active 的账号
// 把按钮禁掉并写「正在使用」。于是两个账号的按钮全都点不动 —— 用户「可以切换，
// 无法切回」，只能去客户端手动退出登录才能解开。
func TestTwoLinesMakeTwoActive(t *testing.T) {
	authD, _ := setupAuthDirs(t)
	const uidA = "aaaa1111-0000-0000-0000-000000000001"
	const uidB = "bbbb2222-0000-0000-0000-000000000002"

	// 两个账号都保存过快照
	writeInfo(t, snapPath(uidA, "workbuddy-desktop"), uidA, 30*24*time.Hour, 0)
	writeInfo(t, snapPath(uidB, "workbuddy-desktop"), uidB, 30*24*time.Hour, 0)

	// 主线路已经是 B，另一条受管线路还留着 A（切号切了一半的残留）
	writeInfo(t, filepath.Join(authD, "workbuddy-desktop.info"), uidB, 30*24*time.Hour, 0)
	writeInfo(t, filepath.Join(authD, "workbuddy-desktop-v2.info"), uidA, 30*24*time.Hour, 0)

	list := accounts()
	actives := 0
	for _, a := range list {
		if a.Active {
			actives++
		}
	}
	if actives > 1 {
		t.Fatalf("出现 %d 个「正在使用」，前端会把按钮全禁掉，用户无法切换", actives)
	}
	// 主线路是 B，所以 active 必须是 B 而不是 A
	for _, a := range list {
		if a.Active && a.UID != uidB {
			t.Fatalf("当前账号判成了 %s，应以主线路 %s 为准", a.UID, uidB)
		}
	}
}

// TestSwitchGuardUsesAuthoritativeLine 复现 issue #1 的「可以切换，无法切回」。
//
// 根因：切换前置检查遍历 auth 目录下所有活动线路，只要有一条指向目标账号就
// 返回「当前已经在这个账号上，无需切换」。而切号残留恰好让旧账号仍留在某条
// 线路上，于是用户点「切回账号1」永远拿到这句话，只能去客户端手动退出登录。
func TestSwitchGuardUsesAuthoritativeLine(t *testing.T) {
	authD, _ := setupAuthDirs(t)
	const uidA = "aaaa1111-0000-0000-0000-000000000011"
	const uidB = "bbbb2222-0000-0000-0000-000000000012"

	writeInfo(t, snapPath(uidA, "workbuddy-desktop"), uidA, 30*24*time.Hour, 0)
	writeInfo(t, snapPath(uidB, "workbuddy-desktop"), uidB, 30*24*time.Hour, 0)
	// 主线路 B，副线路残留 A
	writeInfo(t, filepath.Join(authD, "workbuddy-desktop.info"), uidB, 30*24*time.Hour, 0)
	writeInfo(t, filepath.Join(authD, "workbuddy-desktop-v2.info"), uidA, 30*24*time.Hour, 0)

	uid, prod := activeLine()
	if uid != uidB || prod != "workbuddy-desktop" {
		t.Fatalf("当前账号应取主线路 %s，实际 %s/%s", uidB, prod, uid)
	}
	if sameAccount(uidA) {
		t.Fatal("主线路是 B，切到 A 不该被判定成「已经在这个账号上」——这就是切不回去的原因")
	}
	if !sameAccount(uidB) {
		t.Fatal("目标就是当前账号 B，应判定为无需切换")
	}
}

// TestSwitchClearsForeignLines 切号后不能留下指向旧账号的活动线路。
//
// 根因：switchTo 只覆盖「目标账号有快照的那些线路」。若 A 有两条线路的快照、
// B 只有一条，从 A 切到 B 之后第二条线路仍是 A —— 这就是 issue 里「切过去就
// 切不回来」的来源。未覆盖的线路必须进 backup 挪走，不能留在原地。
func TestSwitchClearsForeignLines(t *testing.T) {
	authD, baseD := setupAuthDirs(t)
	const uidA = "aaaa1111-0000-0000-0000-00000000000a"
	const uidB = "bbbb2222-0000-0000-0000-00000000000b"

	// A 覆盖主线路 + 副线路；B 只有主线路
	writeInfo(t, snapPath(uidA, "workbuddy-desktop"), uidA, 30*24*time.Hour, 0)
	writeInfo(t, snapPath(uidA, "workbuddy-desktop-v2"), uidA, 30*24*time.Hour, 0)
	writeInfo(t, snapPath(uidB, "workbuddy-desktop"), uidB, 30*24*time.Hour, 0)

	// 当前在线的是 A，两条线路都在
	writeInfo(t, filepath.Join(authD, "workbuddy-desktop.info"), uidA, 30*24*time.Hour, 0)
	writeInfo(t, filepath.Join(authD, "workbuddy-desktop-v2.info"), uidA, 30*24*time.Hour, 0)

	if err := switchTo(uidB); err != nil {
		t.Fatalf("切换失败：%v", err)
	}

	for _, p := range scanProducts() {
		if p.UID == uidA {
			t.Fatalf("切到 B 之后，%s 仍指向旧账号 A", p.Product)
		}
	}

	// 副线路不是删掉，是挪进 backup —— 万一那条线用户还在别的客户端用，能捞回来
	entries, err := os.ReadDir(filepath.Join(baseD, "backup"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("被挪走的线路应有备份，实际 err=%v n=%d", err, len(entries))
	}
}
