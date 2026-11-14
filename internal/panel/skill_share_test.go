package panel

import (
	"strings"
	"testing"
)

// skill_share（分享技能给好友）必须挂在动作码表里，否则任务中心扫不到待办、
// 一键完成也会直接报「无法自动完成」。
func TestSkillShareActionRegistered(t *testing.T) {
	act := autoActionFor("skill_share")
	if act == nil {
		t.Fatal("skill_share 未注册到 autoActions")
	}
	if act.run == nil {
		t.Fatal("skill_share 的 run 为空")
	}
	if !strings.Contains(act.Desc, "分享") {
		t.Errorf("Desc 未说明分享动作: %q", act.Desc)
	}
}

// 兜底身份必须与常量 id 一致（防止改了常量忘了改兜底，上报出编造 id 不计数）。
func TestSkillShareFallbackIdentity(t *testing.T) {
	if skillShareFallback.SkillID != skillShareSkillID {
		t.Errorf("兜底 SkillID=%q 与常量 %q 不一致", skillShareFallback.SkillID, skillShareSkillID)
	}
	if !strings.HasPrefix(skillShareSkillID, "skill_") || len(skillShareSkillID) < 12 {
		t.Errorf("技能 id 形状可疑: %q", skillShareSkillID)
	}
	if skillShareFallback.DisplayName() == "" {
		t.Error("兜底技能缺少展示名")
	}
}
