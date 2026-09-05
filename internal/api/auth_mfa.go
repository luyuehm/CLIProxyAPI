package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"cpa-usage-keeper/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

const (
	// mfaPendingCookieTTL 是密码通过后等待 TOTP 二次验证的窗口，超时后需重新输入密码。
	mfaPendingCookieTTL = 5 * time.Minute
	// mfaSetupSecretTTL 是 setup 生成密钥的有效期；前端在此窗口内完成绑定确认。
	mfaSetupSecretTTL = 10 * time.Minute
)

// MFASecretStore 抽象 TOTP 密钥的持久化。实现见 internal/repository/mfa.go，
// 通过 app_settings 表保存管理员 TOTP 配置。
type MFASecretStore interface {
	GetTOTPConfig() (auth.TOTPConfig, error)
	SaveTOTPConfig(auth.TOTPConfig) error
}

type mfaSetupResponse struct {
	Secret    string `json:"secret"`
	OTPURL    string `json:"otp_url"`
	Label     string `json:"label"`
	ExpiresIn int    `json:"expires_in,omitempty"`
}

type mfaEnableRequest struct {
	Secret string `json:"secret"`
	Code   string `json:"code"`
}

type mfaEnableResponse struct {
	Enabled bool `json:"enabled"`
}

type mfaVerifyRequest struct {
	Code string `json:"code"`
}

type mfaVerifyResponse struct {
	SessionToken string `json:"session_token,omitempty"`
	Authenticated bool  `json:"authenticated"`
}

// mfaSetup 在管理员已登录时生成一次性的 TOTP 密钥与 otpauth URI，
// 供个人设置页扫码绑定。确认启用必须走 mfaEnable。
func (h *authHandler) mfaSetup(c *gin.Context) {
	if h == nil || !h.config.Enabled || h.sessions == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "authentication required"})
		return
	}
	_, session, ok := h.resolveValidSession(c)
	if !ok || session.Role != auth.RoleAdmin {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		writeInternalError(c, "generate mfa secret failed", err)
		return
	}
	label := auth.TOTPDefaultAccount
	uri := auth.BuildTOTPURI("LicenseAdmin", label, secret)

	c.JSON(http.StatusOK, mfaSetupResponse{
		Secret:    secret,
		OTPURL:    uri,
		Label:     label,
		ExpiresIn: int(mfaSetupSecretTTL.Seconds()),
	})
}

// mfaEnable 校验新生成的 TOTP 密钥与动态码，成功后持久化启用 2FA。
// 需要管理员会话。Code 必须与传入 secret 在 ±1 个周期内匹配。
func (h *authHandler) mfaEnable(c *gin.Context) {
	if h == nil || !h.config.Enabled || h.sessions == nil || h.mfaStore == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "authentication required"})
		return
	}
	_, session, ok := h.resolveValidSession(c)
	if !ok || session.Role != auth.RoleAdmin {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var request mfaEnableRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		if isRequestEntityTooLarge(err) {
			writeRequestEntityTooLarge(c)
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	secret := strings.ToUpper(strings.TrimSpace(request.Secret))
	if !auth.IsValidTOTPSecret(secret) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid mfa secret"})
		return
	}
	code := auth.NormalizeTOTPCode(request.Code)
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "verification code is required"})
		return
	}
	if !auth.ValidateTOTPCode(secret, code, time.Now()) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid verification code"})
		return
	}

	if err := h.mfaStore.SaveTOTPConfig(auth.TOTPConfig{
		Secret:      secret,
		Enabled:     true,
		ConfirmedAt: time.Now(),
	}); err != nil {
		logrus.WithError(err).Warn("save totp config failed")
		writeInternalError(c, "enable mfa failed", err)
		return
	}
	c.JSON(http.StatusOK, mfaEnableResponse{Enabled: true})
}

// mfaVerify 完成登录的第二步：持有密码阶段的 pending MFA Cookie，
// 校验已启用的 TOTP 动态码后创建正式会话。
func (h *authHandler) mfaVerify(c *gin.Context) {
	if h == nil || !h.config.Enabled || h.sessions == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "authentication required"})
		return
	}
	pendingValue, err := c.Cookie(pendingMFACookieName)
	if err != nil || pendingValue == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "mfa challenge expired"})
		return
	}
	mfaConfig, ok := h.loadTOTPConfig(c)
	if !ok {
		clearPendingMFACookie(c, h.config.BasePath, resolveSessionToken(c).CookieKind)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "mfa not configured"})
		return
	}
	if !mfaConfig.Enabled || mfaConfig.Secret == "" {
		clearPendingMFACookie(c, h.config.BasePath, resolveSessionToken(c).CookieKind)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "mfa not configured"})
		return
	}

	var request mfaVerifyRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		if isRequestEntityTooLarge(err) {
			writeRequestEntityTooLarge(c)
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	code := auth.NormalizeTOTPCode(request.Code)
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "verification code is required"})
		return
	}

	resolved := resolveSessionToken(c)
	if !auth.ValidateTOTPCode(mfaConfig.Secret, code, time.Now()) {
		// 校验失败保留待验证状态，允许重试；不暴露内部细节。
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid verification code"})
		return
	}

	// 二次验证通过：创建正式会话并清除待验证标记。
	// Remember Me 意图在密码阶段写入 pending cookie 时锁定，这里原样沿用。
	rememberMe := rememberMePendingMarker(pendingValue)
	clearPendingMFACookie(c, h.config.BasePath, resolved.CookieKind)
	token, expiresAt, err := h.createAdminSession(c, resolved, rememberMe)
	if err != nil {
		writeInternalError(c, "create auth session failed", err)
		return
	}
	setSessionCookie(c, h.config.BasePath, resolved.CookieKind, token, expiresAt)

	if resolved.Source == auth.SessionSourceEmbed {
		c.JSON(http.StatusOK, mfaVerifyResponse{Authenticated: true, SessionToken: token})
		return
	}
	c.JSON(http.StatusOK, mfaVerifyResponse{Authenticated: true})
}

// loadTOTPConfig 读取持久化 TOTP 配置。从未配置过时返回零值配置且 ok=true。
func (h *authHandler) loadTOTPConfig(c *gin.Context) (auth.TOTPConfig, bool) {
	if h == nil || h.mfaStore == nil {
		return auth.TOTPConfig{}, false
	}
	config, err := h.mfaStore.GetTOTPConfig()
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return auth.TOTPConfig{}, true
		}
		logrus.WithError(err).Warn("load totp config failed")
		return auth.TOTPConfig{}, false
	}
	return config, true
}