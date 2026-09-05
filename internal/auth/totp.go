package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// TOTP 参数遵循 RFC 6238：30 秒周期、6 位动态码、SHA-1 HMAC。
// Authenticator 类应用默认与这些参数兼容。
const (
	totpPeriodSeconds = 30
	totpDigits        = 6
)

// TOTPSecretSizeBytes 是生成的 Base32 密钥的熵长度（160 位）。
const TOTPSecretSizeBytes = 20

// TOTPDefaultAccount 是管理台 2FA 绑定的默认账号标识。
const TOTPDefaultAccount = "admin"

// IsValidTOTPSecret 校验密钥是否为可解码的无填充 Base32，且具有合理熵长度。
func IsValidTOTPSecret(secret string) bool {
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return false
	}
	return len(decoded) >= 10
}

// TOTPSetup 是一次 TOTP 绑定流程所需的全部数据。
type TOTPSetup struct {
	Secret    string `json:"secret"`
	OTPSecret string `json:"otp_secret"`
	OTPURL    string `json:"otp_url"`
	Label     string `json:"label"`
}

// TOTPConfig 持久化的 TOTP 绑定状态。Secret 已校验可用后才会写入。
type TOTPConfig struct {
	Secret      string
	Enabled     bool
	ConfirmedAt time.Time
}

// GenerateTOTPSecret 生成 20 字节随机密钥，并返回无填充的标准 Base32 编码。
func GenerateTOTPSecret() (string, error) {
	buf := make([]byte, TOTPSecretSizeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate totp secret: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// BuildTOTPURI 构建 otpauth 迁移 URI。issuer/account 会做 URL 编码。
// 示例：otpauth://totp/LicenseAdmin:admin?secret=...&issuer=LicenseAdmin&algorithm=SHA1&digits=6&period=30
func BuildTOTPURI(issuer, account, secret string) string {
	if issuer == "" {
		issuer = "LicenseAdmin"
	}
	label := issuer
	if account != "" {
		label = issuer + ":" + account
	}
	query := fmt.Sprintf("secret=%s&issuer=%s&algorithm=SHA1&digits=%d&period=%d",
		secret,
		urlEncodeComponent(issuer),
		totpDigits,
		totpPeriodSeconds,
	)
	return fmt.Sprintf("otpauth://totp/%s?%s", urlEncodePathComponent(label), query)
}

// TOTPCodeAt 计算某个时间点的 TOTP 验证码（RFC 6238 TOTP）。
func TOTPCodeAt(secret string, at time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("decode totp secret: %w", err)
	}
	counter := uint64(at.Unix() / totpPeriodSeconds)
	mac := hmac.New(sha1.New, key)
	var counterBytes [8]byte
	binary.BigEndian.PutUint64(counterBytes[:], counter)
	_, _ = mac.Write(counterBytes[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])
	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, code%mod), nil
}

// ValidateTOTPCode 校验动态码，允许 ±1 个周期的时间窗偏移以容忍时钟漂移。
func ValidateTOTPCode(secret, code string, at time.Time) bool {
	expected := strings.TrimSpace(code)
	if expected == "" {
		return false
	}
	for delta := int64(-1); delta <= 1; delta++ {
		candidate, err := TOTPCodeAt(secret, at.Add(time.Duration(delta)*totpPeriodSeconds*time.Second))
		if err != nil {
			return false
		}
		if hmac.Equal([]byte(candidate), []byte(expected)) {
			return true
		}
	}
	return false
}

// NormalizeTOTPCode 去除动态码中的空格与连字符，便于宽松输入。
func NormalizeTOTPCode(code string) string {
	replacer := strings.NewReplacer(" ", "", "-", "")
	return strings.TrimSpace(replacer.Replace(code))
}

// urlEncodePathComponent 编码 otpauth URI 路径段（label）。
func urlEncodePathComponent(value string) string {
	const upperHex = "0123456789ABCDEF"
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '~' {
			b.WriteRune(r)
			continue
		}
		for _, c := range []byte(string(r)) {
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0f])
		}
	}
	return b.String()
}

// urlEncodeComponent 编码 otpauth URI 查询参数值（issuer）。
func urlEncodeComponent(value string) string {
	const upperHex = "0123456789ABCDEF"
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '~' {
			b.WriteRune(r)
			continue
		}
		for _, c := range []byte(string(r)) {
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0f])
		}
	}
	return b.String()
}
