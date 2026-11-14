package upstream

import "testing"

func TestDesktopSkillShareEventsShape(t *testing.T) {
	sk := MarketSkill{SkillID: "skill_123", DisplayNameZH: "测试技能", Source: "others"}
	evs := DesktopSkillShareEvents(sk, "link")
	if len(evs) != 3 {
		t.Fatalf("events=%d, want 3", len(evs))
	}
	wantCodes := []string{"skill_detail_show", "skill_share_entry_click", "skill_share_channel_action"}
	for i, want := range wantCodes {
		if got := evs[i]["eventCode"]; got != want {
			t.Errorf("event[%d].eventCode=%v, want %s", i, got, want)
		}
	}
	act := evs[2]
	if act["skillId"] != "skill_123" || act["skillName"] != "测试技能" {
		t.Errorf("身份字段错: %v / %v", act["skillId"], act["skillName"])
	}
	if act["skillType"] != "market" || act["skillSource"] != "others" {
		t.Errorf("类型字段错: %v / %v", act["skillType"], act["skillSource"])
	}
	// 判据要点：isSuccessful=true + panelAction=success，否则不计数。
	if act["isSuccessful"] != true || act["panelAction"] != "success" {
		t.Errorf("成功标记错: %v / %v", act["isSuccessful"], act["panelAction"])
	}
	if act["action"] != "link" || act["panelType"] != "clipboard" {
		t.Errorf("渠道映射错: %v / %v", act["action"], act["panelType"])
	}
	if _, ok := act["actionAt"].(int64); !ok {
		t.Errorf("actionAt 缺失或类型错: %T", act["actionAt"])
	}
}

func TestSkillSharePanelTypeMapping(t *testing.T) {
	cases := map[string]string{
		"wechat":  "mini_program_card",
		"moments": "mini_program_card",
		"qrcode":  "qrcode_image",
		"browser": "external_browser",
		"link":    "clipboard",
		"":        "clipboard",
	}
	for ch, want := range cases {
		if got := skillSharePanelType(ch); got != want {
			t.Errorf("channel %q -> %q, want %q", ch, got, want)
		}
	}
}

func TestMarketSkillDisplayNameFallback(t *testing.T) {
	if got := (MarketSkill{SkillID: "skill_1", Name: "en", DisplayNameZH: "中文"}).DisplayName(); got != "中文" {
		t.Errorf("中文优先失败: %q", got)
	}
	if got := (MarketSkill{SkillID: "skill_1", Name: "en"}).DisplayName(); got != "en" {
		t.Errorf("name 回落失败: %q", got)
	}
	if got := (MarketSkill{SkillID: "skill_1"}).DisplayName(); got != "skill_1" {
		t.Errorf("id 回落失败: %q", got)
	}
}
