// job.go 让界面窗口跟着管家一起消失。
//
// Edge 是用 --app= 拉起来的独立进程，管家退出时正常路径会主动 kill 它，
// 但如果管家被任务管理器强杀、或者崩掉，这个窗口就没人管了——变成一个
// 打不开页面、也关不掉的孤儿窗口。页面里的 window.close() 对 --app 窗口
// 被 Chromium 拒绝，只能从系统层面解决：把 Edge 放进一个 Job Object，
// 并设置 KILL_ON_JOB_CLOSE，管家进程消失时内核会连带终止它。
package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var edgeJob windows.Handle

// bindBrowserToJob 把指定进程关进"随管家一起死"的 Job。
func bindBrowserToJob(pid int) error {
	if edgeJob == 0 {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return err
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(
			h,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)),
		); err != nil {
			windows.CloseHandle(h)
			return err
		}
		edgeJob = h
	}
	ph, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(ph)
	return windows.AssignProcessToJobObject(edgeJob, ph)
}

// jobBasicAccounting 对应 Win32 的 JOBOBJECT_BASIC_ACCOUNTING_INFORMATION。
// x/sys/windows 只给了信息类的常量，没给结构体，这里按 winnt.h 的布局声明：
// 4 个 int64 之后是 4 个 uint32，共 48 字节。
type jobBasicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// jobActiveCount 返回 Job 里还活着的进程数；第二个返回值表示这次查询是否可信
// （Job 没建起来、或查询失败时为 false，调用方要退回单 pid 判断）。
//
// 为什么需要它：Edge 首次初始化一个全新的 user-data-dir 时，会把自己 relaunch
// 一次——旧主进程立刻退出、新进程接管窗口。管家只盯着当初 cmd.Start() 拿到的
// 那个 pid，于是判定「窗口已关闭」，转头把整个 Job 连同新窗口一起 kill 掉。
// 用户看到的就是「第一次启动闪退，第二次启动才行」（第二次 profile 已经初始化
// 完，不再 relaunch）。Job 里装的是整棵浏览器进程树，数它才靠谱。
func jobActiveCount() (int, bool) {
	if edgeJob == 0 {
		return 0, false
	}
	var info jobBasicAccounting
	err := windows.QueryInformationJobObject(
		edgeJob,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	)
	if err != nil {
		return 0, false
	}
	return int(info.ActiveProcesses), true
}

// killBrowser 收掉整棵浏览器进程树。
//
// 优先走 Job：relaunch 出来的新主进程、渲染进程、GPU 进程都属于这个 Job，
// 只 Kill 当初那个 pid 会留下关不掉的孤儿窗口。Job 不可用时退回杀主进程。
func killBrowser() {
	if edgeJob != 0 {
		if err := windows.TerminateJobObject(edgeJob, 1); err == nil {
			return
		}
	}
	if browser != nil && browser.Process != nil {
		_ = browser.Process.Kill()
	}
}
