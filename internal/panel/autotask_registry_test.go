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

// ---- 码表外的「召唤指定专家」任务（expert_use_* 族） ----

// jumpURL 拼一条真实形态的任务跳转链接（前半段是 URL 编码的 deeplink）。
func jumpURL(expertID string) string {
	return "https://www.workbuddy.cn/work/launch/?deeplink=workbuddy%3A%2F%2Fexperts%3FexpertId%3D" +
		expertID + "&buddy_type=workbuddy"
}

// TestExpertFromTask 从 jump_url 解出权威专家 id、从标题取名字。
// 必须用上游给的 id：同名专家在市场里可能有多条（实测「中国电商运营专家」搜出来的是
// 另一个 ex_ id），按名字猜会召唤错人、事件不计分。
func TestExpertFromTask(t *testing.T) {
	task := upstream.Task{
		TaskCode: "expert_use_maidehao",
		Title:    "体验「中国电商运营专家」",
		TaskDesc: "在专家页搜索并召唤「中国电商运营专家」，完成一次对话。",
		JumpURL:  jumpURL("ex_GDl31WahyIH0"),
	}
	id, name := expertFromTask(&task)
	if id != "ex_GDl31WahyIH0" {
		t.Fatalf("专家 id 解析错误：%q", id)
	}
	if name != "中国电商运营专家" {
		t.Fatalf("专家名解析错误：%q", name)
	}
}

// TestExpertFromTaskNoID 没有专家 id 的任务不能被误判成专家任务。
func TestExpertFromTaskNoID(t *testing.T) {
	for _, url := range []string{
		"",
		"workbuddy://playbook",
		"https://www.workbuddy.cn/work/launch/?deeplink=workbuddy%3A%2F%2Fplaybook",
	} {
		task := upstream.Task{TaskCode: "Sequential_Tasks_7", Title: "体验灵感功能", JumpURL: url}
		if id, name := expertFromTask(&task); id != "" || name != "" {
			t.Fatalf("不该解析出专家（url=%q）: id=%q name=%q", url, id, name)
		}
	}
}

// TestExpertFromTaskSuffixParam deeplink 后面还挂着别的参数时，id 不能被带脏。
func TestExpertFromTaskSuffixParam(t *testing.T) {
	task := upstream.Task{
		Title:   "体验「抖音策略师」",
		JumpURL: "https://www.workbuddy.cn/work/launch/?deeplink=" + "workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_loEfkMTwHi8v%26from%3Dtask&buddy_type=workbuddy",
	}
	id, _ := expertFromTask(&task)
	if id != "ex_loEfkMTwHi8v" {
		t.Fatalf("id 带上了尾部参数：%q", id)
	}
}

// TestUnknownExpertTaskAutoSupported 码表外的 expert_use_* 任务靠自己的 jump_url
// 就能可自动化——这是「新活动任务上架后又要手动补码表」的根治点。
func TestUnknownExpertTaskAutoSupported(t *testing.T) {
	task := upstream.Task{
		TaskCode:     "expert_use_bowangwang",
		Title:        "体验「直播电商教练」",
		JumpURL:      jumpURL("ex_WoiLzBBnpu8T"),
		AcceptStatus: "not_accepted",
	}
	if autoActionFor(task.TaskCode) != nil {
		t.Skip("该码已在码表中，本用例针对码表外情形")
	}
	act := autoActionForTask(&task)
	if act == nil {
		t.Fatal("码表外的 expert_use_* 任务应能被识别（否则面板不显示、无法自动完成）")
	}
	if act.run == nil {
		t.Fatal("动态动作缺 run 实现")
	}
	if !growthPending(task) {
		t.Fatal("码表外的专家任务应进待办")
	}
}

// TestNoAutoTasksStayExcluded 明确不做的任务（要真实关注公众号/真实捐款）
// 即使挂着专家 id 也不能被动态动作捞进来。
func TestNoAutoTasksStayExcluded(t *testing.T) {
	for code := range noAutoTasks {
		task := upstream.Task{TaskCode: code, Title: "体验「某专家」", JumpURL: jumpURL("ex_abc123")}
		if autoActionForTask(&task) != nil {
			t.Fatalf("%s 不应生成自动动作", code)
		}
		if growthPending(task) {
			t.Fatalf("%s 不应进待办", code)
		}
	}
}

// TestNilTaskNoAction 空任务不能 panic（扫描时 items 可能为空槽位）。
func TestNilTaskNoAction(t *testing.T) {
	if autoActionForTask(nil) != nil {
		t.Fatal("nil 任务不该解析出动作")
	}
	if id, name := expertFromTask(nil); id != "" || name != "" {
		t.Fatal("nil 任务不该解析出专家")
	}
}
