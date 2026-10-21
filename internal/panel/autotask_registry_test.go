package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// taskStub 构造一条「未完成、未锁定、进度 0」的任务，用于验证待办过滤。
func taskStub(code string) upstream.Task {
	return upstream.Task{TaskCode: code, AcceptStatus: "accepted"}
}

// TestAutoActionsNoDuplicateCode 任务码表不允许重复条目。
// 重复会让 autoActionFor 静默命中前一条，后加的实现永远不生效——最难查的那类 bug。
func TestAutoActionsNoDuplicateCode(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range autoActions {
		if a.TaskCode == "" {
			t.Fatalf("存在空 TaskCode 的条目（Desc=%q）", a.Desc)
		}
		if seen[a.TaskCode] {
			t.Fatalf("任务码重复：%s", a.TaskCode)
		}
		seen[a.TaskCode] = true
		if a.run == nil {
			t.Fatalf("任务 %s 没有 run 实现", a.TaskCode)
		}
		if a.Desc == "" {
			t.Fatalf("任务 %s 没有描述（面板会显示空行）", a.TaskCode)
		}
	}
}

// TestAutoActionsEveryEntryReachable 每个条目都必须能被 autoActionFor 查到，
// 且 growthPending 对其返回 true（未完成、未锁定）。这是「新任务识别不出来」的直接回归点：
// 码表里漏一条，该任务就永远不进待办。
func TestAutoActionsEveryEntryReachable(t *testing.T) {
	for _, a := range autoActions {
		if autoActionFor(a.TaskCode) == nil {
			t.Fatalf("autoActionFor 查不到 %s", a.TaskCode)
		}
		if !growthPending(taskStub(a.TaskCode)) {
			t.Fatalf("%s 应进待办（未完成、未锁定、有动作）", a.TaskCode)
		}
	}
}

// TestValorantTaskRegistered WorkBuddy_Valorant 是 2026-10 才下发的活动任务，
// 之前不在码表里导致面板完全不显示。这里锁死它必须存在且可执行。
func TestValorantTaskRegistered(t *testing.T) {
	a := autoActionFor("WorkBuddy_Valorant")
	if a == nil {
		t.Fatal("WorkBuddy_Valorant 未登记，面板不会显示该任务")
	}
	if a.run == nil {
		t.Fatal("WorkBuddy_Valorant 无 run 实现")
	}
	if !growthPending(taskStub("WorkBuddy_Valorant")) {
		t.Fatal("WorkBuddy_Valorant 未进待办")
	}
}

// TestGrowthPendingUnknownCodeExcluded 未登记的任务码不进待办——
// 否则队列执行时会因 autoActionFor 为 nil 报错。
func TestGrowthPendingUnknownCodeExcluded(t *testing.T) {
	if growthPending(taskStub("Some_Unknown_Task_2099")) {
		t.Fatal("未登记的任务码不应进待办")
	}
}

// TestGrowthPendingClaimedExcluded 已领取的任务不进待办。
func TestGrowthPendingClaimedExcluded(t *testing.T) {
	task := taskStub("WorkBuddy_Valorant")
	task.Claimed = true
	if growthPending(task) {
		t.Fatal("已领取任务不应进待办")
	}
}

// TestGrowthPendingLockedExcluded 上游锁定的任务不进待办（每日零点解锁）。
func TestGrowthPendingLockedExcluded(t *testing.T) {
	task := taskStub("WorkBuddy_Valorant")
	task.Locked = true
	if growthPending(task) {
		t.Fatal("已锁定任务不应进待办")
	}
}

// TestGrowthPendingReachedTargetStillQueued 达标未领取的任务仍需入队（执行时会自动领）。
func TestGrowthPendingReachedTargetStillQueued(t *testing.T) {
	task := taskStub("WorkBuddy_Valorant")
	task.Target = 1
	task.Current = 1
	if !growthPending(task) {
		t.Fatal("达标未领取的任务仍应入队（执行后自动领取）")
	}
}
