// skill.go 技能市场 + 「分享技能」判据事件。
//
// 2026-11-14 逆向与实测（客户端 5.9.5 asar）：
//   - 市场详情：POST /portal/operation-platform/market/skill/get-by-ids
//     body {"skill_ids":[...]} → data.skills[]（skill_id / name / display_name_zh /
//     version / source）。分享事件要求 skillId 是平台真实存在的技能，
//     故先查详情再上报。
//   - 判据事件：`skill_share_channel_action`（analytics，走 /v2/report），
//     字段 {skillId, skillName, skillType, skillSource, action, panelAction,
//     panelType, isSuccessful, cost, id, actionAt}。
//     实测两账号：单发这一条即 0/0 → 1/1 点亮，无需真实分享码 / 无需好友收件，
//     客户端版本门（5.9.0+）只在前端做，服务端不校验（指纹 ideVersion 仍是 5.5.6 也计数）。
//   - 真实客户端链路会先报 skill_detail_show（进详情页）再报
//     skill_share_entry_click（点分享）最后 channel_action（选渠道），这里按同序补齐，
//     渠道用 link（面板类型 clipboard，最接近「复制链接」这条不依赖微信的路径）。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// marketSkillByIDsPath 技能市场批量详情（客户端「技能」列表进详情页时调用）。
const marketSkillByIDsPath = "/portal/operation-platform/market/skill/get-by-ids"

// MarketSkill 技能市场里的一条技能（响应子集）。
type MarketSkill struct {
	SkillID       string `json:"skill_id"`
	Name          string `json:"name"`
	DisplayNameZH string `json:"display_name_zh"`
	Version       string `json:"version"`
	Source        string `json:"source"`
}

// DisplayName 展示名：中文名优先，回落英文 name，最后回落 id。
func (s MarketSkill) DisplayName() string {
	switch {
	case s.DisplayNameZH != "":
		return s.DisplayNameZH
	case s.Name != "":
		return s.Name
	default:
		return s.SkillID
	}
}

// MarketSkillByIDs 按 id 批量查技能详情（market 域走 chatBase，与专家市场同域）。
func (c *Client) MarketSkillByIDs(a *auth.Auth, ids []string) ([]MarketSkill, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("skill: empty ids")
	}
	raw, err := json.Marshal(map[string]any{"skill_ids": ids})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+marketSkillByIDsPath, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Domain", c.chatBase(a))
	req.Header.Set("X-Product", "SaaS")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	var out struct {
		Skills []MarketSkill `json:"skills"`
	}
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("skill detail parse: %w", err)
	}
	return out.Skills, nil
}

// skillSharePanelType 分享渠道 → 客户端面板类型（asar 里的映射表）。
func skillSharePanelType(channel string) string {
	switch channel {
	case "wechat", "moments":
		return "mini_program_card"
	case "qrcode":
		return "qrcode_image"
	case "browser":
		return "external_browser"
	default: // link
		return "clipboard"
	}
}

// DesktopSkillShareEvents 构造「进技能详情 → 点分享 → 选渠道」的事件链。
// skillType/skillSource 与客户端一致：市场技能 type=market，source 取详情里的
// source 字段（回落 market）。
func DesktopSkillShareEvents(skill MarketSkill, channel string) []DesktopEvent {
	if channel == "" {
		channel = "link"
	}
	source := skill.Source
	if source == "" {
		source = "market"
	}
	id := skill.SkillID
	name := skill.DisplayName()
	base := func(code string) DesktopEvent {
		return DesktopEvent{
			"eventCode": code, "skillId": id, "skillName": name,
			"skillType": "market", "skillSource": source,
		}
	}
	show := base("skill_detail_show")
	show["source"] = "skills_list"

	entry := base("skill_share_entry_click")
	entry["entry"] = "detail"
	entry["status"] = "safe"
	entry["actionSuccessful"] = true
	entry["tooltip"] = ""
	entry["clickAt"] = time.Now().UnixMilli()

	// 这一条是判据本身（实测单发即点亮）。
	act := base("skill_share_channel_action")
	act["action"] = channel
	act["panelAction"] = "success"
	act["panelType"] = skillSharePanelType(channel)
	act["isSuccessful"] = true
	act["cost"] = 0
	act["id"] = ""
	act["actionAt"] = time.Now().UnixMilli()

	return []DesktopEvent{show, entry, act}
}
