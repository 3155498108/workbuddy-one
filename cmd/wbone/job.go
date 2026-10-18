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
