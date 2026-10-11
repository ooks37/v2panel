package panel

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// 面板级开关的读写。
//
// 这里刻意只放**影响用户体验的少数开关**, 不是通用 KV 编辑器 ——
// 通用编辑器会让配置漂移, 出问题时没人知道哪一项被改过。

// 订阅密码保护的开关键。开启后 /sub/<token> 需要密码, 链接要带 ?pwd=xxx。
const settingSubRequirePwd = "sub_require_password"

// settingsView 是面板设置的回给前端的形态。
type settingsView struct {
	// 订阅密码保护: 默认关闭。
	// 关闭时订阅 Token 是唯一凭据 (对用户最省事);
	// 开启后门户要输密码, 防的是"订阅链接被转发出去"。
	SubRequirePwd bool `json:"sub_require_password"`

	// 面板对外域名 (含 scheme), 订阅链接与二维码用它。
	PanelDomain string `json:"panel_domain"`
}

func loadSettings() settingsView {
	return settingsView{
		SubRequirePwd: strings.TrimSpace(GetSetting(settingSubRequirePwd, "0")) == "1",
		PanelDomain:   strings.TrimSpace(GetSetting("panel_domain", "")),
	}
}

func HandleGetSettings(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": loadSettings()})
}

type settingsUpdateRequest struct {
	SubRequirePwd *bool   `json:"sub_require_password"`
	PanelDomain   *string `json:"panel_domain"`
}

// HandleUpdateSettings 更新面板设置。
//
// 用指针区分「没传」和「传了 false/空串」—— 关掉开关是常见操作,
// 用值类型会被 GORM 的零值逻辑静默吞掉。
func HandleUpdateSettings(c *gin.Context) {
	var req settingsUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "请求参数格式错误"})
		return
	}

	if req.SubRequirePwd != nil {
		v := "0"
		if *req.SubRequirePwd {
			v = "1"
		}
		if err := SetSetting(settingSubRequirePwd, v); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "保存失败: " + err.Error()})
			return
		}
	}

	if req.PanelDomain != nil {
		d := strings.TrimSpace(*req.PanelDomain)
		// 只接受空串 (清除) 或 http(s)://host 形式。这个值会被拼进
		// 订阅链接、二维码文本, 直接回给用户, 所以在这里收口。
		if d != "" {
			if !strings.HasPrefix(d, "http://") && !strings.HasPrefix(d, "https://") {
				d = "https://" + d
			}
			host := d
			for _, p := range []string{"https://", "http://"} {
				host = strings.TrimPrefix(host, p)
			}
			host = strings.TrimRight(host, "/")
			if host == "" || strings.ContainsAny(host, " \t\"'<>`\\") {
				c.JSON(http.StatusBadRequest, gin.H{"code": 400, "error": "BadRequest", "message": "域名格式不合法"})
				return
			}
			d = strings.TrimRight(d, "/")
		}
		if err := SetSetting("panel_domain", d); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "error": "InternalError", "message": "保存失败: " + err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "设置已保存", "data": loadSettings()})
}
