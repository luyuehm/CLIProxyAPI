package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cpa-usage-keeper/internal/auth"
)

// memoryMFASecretStore 是 MFA 持久化的内存实现，用于 handler 测试。
type memoryMFASecretStore struct {
	config auth.TOTPConfig
	ok     bool
}

func (s *memoryMFASecretStore) GetTOTPConfig() (auth.TOTPConfig, error) {
	if !s.ok {
		return auth.TOTPConfig{}, errMFANotFound
	}
	return s.config, nil
}

func (s *memoryMFASecretStore) SaveTOTPConfig(config auth.TOTPConfig) error {
	s.config = config
	s.ok = true
	return nil
}

var errMFANotFound = &mfaNotFoundError{}

type mfaNotFoundError struct{}

func (*mfaNotFoundError) Error() string { return "mfa not found" }

func newMFAHandlerForTest(config AuthConfig, store MFASecretStore) *authHandler {
	handler := NewAuthHandler(config, auth.NewSessionManager(config.SessionTTL))
	if store != nil {
		handler.SetMFASecretStore(store)
	}
	return handler
}

func TestMFASetupRequiresAuthenticatedAdmin(t *testing.T) {
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	handler := newMFAHandlerForTest(config, &memoryMFASecretStore{})
	router := NewRouter(nil, nil, nil, nil, config, handler, "")

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/mfa/setup", nil)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected setup without session to be 401, got %d %s", resp.Code, resp.Body.String())
	}
}

func TestMFASetupReturnsSecretAndOTPURLForAdmin(t *testing.T) {
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	handler := newMFAHandlerForTest(config, &memoryMFASecretStore{})
	router := NewRouter(nil, nil, nil, nil, config, handler, "")

	loginResp := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"secret"}`))
	loginReq.Header.Set(requestIntentHeaderName, requestIntentHeaderValueFetch)
	loginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(loginResp, loginReq)
	if loginResp.Code != http.StatusNoContent {
		t.Fatalf("expected login 204, got %d", loginResp.Code)
	}
	cookie := loginResp.Result().Cookies()[0]

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/mfa/setup", nil)
	req.AddCookie(cookie)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected setup 200, got %d %s", resp.Code, resp.Body.String())
	}
	var parsed struct {
		Secret string `json:"secret"`
		OTPURL string `json:"otp_url"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode setup response: %v", err)
	}
	if parsed.Secret == "" || !strings.HasPrefix(parsed.OTPURL, "otpauth://totp/") || !strings.Contains(parsed.OTPURL, "secret="+parsed.Secret) {
		t.Fatalf("unexpected setup response: %+v", parsed)
	}
}

func TestMFAEnablePersistsConfiguredTOTP(t *testing.T) {
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	store := &memoryMFASecretStore{}
	handler := newMFAHandlerForTest(config, store)
	router := NewRouter(nil, nil, nil, nil, config, handler, "")

	loginResp := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"secret"}`))
	loginReq.Header.Set(requestIntentHeaderName, requestIntentHeaderValueFetch)
	loginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(loginResp, loginReq)
	cookie := loginResp.Result().Cookies()[0]

	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	code, err := auth.TOTPCodeAt(secret, time.Now())
	if err != nil {
		t.Fatalf("TOTPCodeAt: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/enable", strings.NewReader(`{"secret":"`+secret+`","code":"`+code+`"}`))
	req.Header.Set(requestIntentHeaderName, requestIntentHeaderValueFetch)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected enable 200, got %d %s", resp.Code, resp.Body.String())
	}
	if !store.ok || !store.config.Enabled || store.config.Secret != secret {
		t.Fatalf("expected TOTP config to be persisted, got %+v ok=%v", store.config, store.ok)
	}
}

func TestMFAEnableRejectsWrongCode(t *testing.T) {
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	store := &memoryMFASecretStore{}
	handler := newMFAHandlerForTest(config, store)
	router := NewRouter(nil, nil, nil, nil, config, handler, "")

	loginResp := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"secret"}`))
	loginReq.Header.Set(requestIntentHeaderName, requestIntentHeaderValueFetch)
	loginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(loginResp, loginReq)
	cookie := loginResp.Result().Cookies()[0]

	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/enable", strings.NewReader(`{"secret":"`+secret+`","code":"000000"}`))
	req.Header.Set(requestIntentHeaderName, requestIntentHeaderValueFetch)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected wrong code to be 401, got %d %s", resp.Code, resp.Body.String())
	}
	if store.ok {
		t.Fatal("expected TOTP config not to be persisted on failed enable")
	}
}

func TestMFALoginChallengeAndVerify(t *testing.T) {
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	store := &memoryMFASecretStore{}
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	store.config = auth.TOTPConfig{Secret: secret, Enabled: true, ConfirmedAt: time.Now()}
	store.ok = true

	handler := newMFAHandlerForTest(config, store)
	router := NewRouter(nil, nil, nil, nil, config, handler, "")

	// 密码正确但开启了 2FA：应返回 requires_mfa 而不是直接建会话。
	loginResp := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"secret"}`))
	loginReq.Header.Set(requestIntentHeaderName, requestIntentHeaderValueFetch)
	loginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(loginResp, loginReq)
	if loginResp.Code != http.StatusOK {
		t.Fatalf("expected login challenge 200, got %d %s", loginResp.Code, loginResp.Body.String())
	}
	if !strings.Contains(loginResp.Body.String(), `"requires_mfa":true`) {
		t.Fatalf("expected requires_mfa in login response, got %s", loginResp.Body.String())
	}
	pendingCookie := loginResp.Result().Cookies()
	if len(pendingCookie) == 0 || pendingCookie[0].Name != pendingMFACookieName {
		t.Fatalf("expected pending MFA cookie, got %+v", pendingCookie)
	}

	code, err := auth.TOTPCodeAt(secret, time.Now())
	if err != nil {
		t.Fatalf("TOTPCodeAt: %v", err)
	}

	verifyResp := httptest.NewRecorder()
	verifyReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", strings.NewReader(`{"code":"`+code+`"}`))
	verifyReq.Header.Set(requestIntentHeaderName, requestIntentHeaderValueFetch)
	verifyReq.Header.Set("Content-Type", "application/json")
	verifyReq.AddCookie(pendingCookie[0])
	router.ServeHTTP(verifyResp, verifyReq)

	if verifyResp.Code != http.StatusOK {
		t.Fatalf("expected verify 200, got %d %s", verifyResp.Code, verifyResp.Body.String())
	}
	sessionCookies := verifyResp.Result().Cookies()
	var gotSessionCookie *http.Cookie
	for _, cookie := range sessionCookies {
		if cookie.Name == sessionCookieName {
			gotSessionCookie = cookie
		}
	}
	if gotSessionCookie == nil || gotSessionCookie.Value == "" {
		t.Fatalf("expected real session cookie after MFA verify, got %+v", sessionCookies)
	}

	// 二次验证后的会话应能访问受保护路由。
	usageResp := httptest.NewRecorder()
	usageReq := httptest.NewRequest(http.MethodGet, "/api/v1/usage/overview", nil)
	usageReq.AddCookie(gotSessionCookie)
	router.ServeHTTP(usageResp, usageReq)
	if usageResp.Code != http.StatusOK {
		t.Fatalf("expected protected route to succeed after MFA verify, got %d %s", usageResp.Code, usageResp.Body.String())
	}
}

func TestMFAVerifyRequiresPendingChallengeCookie(t *testing.T) {
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	store := &memoryMFASecretStore{}
	secret, _ := auth.GenerateTOTPSecret()
	store.config = auth.TOTPConfig{Secret: secret, Enabled: true, ConfirmedAt: time.Now()}
	store.ok = true

	handler := newMFAHandlerForTest(config, store)
	router := NewRouter(nil, nil, nil, nil, config, handler, "")

	code, _ := auth.TOTPCodeAt(secret, time.Now())
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", strings.NewReader(`{"code":"`+code+`"}`))
	req.Header.Set(requestIntentHeaderName, requestIntentHeaderValueFetch)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized || !strings.Contains(resp.Body.String(), "mfa challenge expired") {
		t.Fatalf("expected 401 challenge expired without pending cookie, got %d %s", resp.Code, resp.Body.String())
	}
}

func TestMFALoginRememberMeSurvivesSecondStep(t *testing.T) {
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	store := &memoryMFASecretStore{}
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	store.config = auth.TOTPConfig{Secret: secret, Enabled: true, ConfirmedAt: time.Now()}
	store.ok = true

	handler := newMFAHandlerForTest(config, store)
	router := NewRouter(nil, nil, nil, nil, config, handler, "")

	loginResp := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"secret","rememberMe":true}`))
	loginReq.Header.Set(requestIntentHeaderName, requestIntentHeaderValueFetch)
	loginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(loginResp, loginReq)
	var pendingCookie *http.Cookie
	for _, cookie := range loginResp.Result().Cookies() {
		if cookie.Name == pendingMFACookieName {
			pendingCookie = cookie
		}
	}
	if pendingCookie == nil {
		t.Fatal("expected pending MFA cookie")
	}
	if pendingCookie.Value != "rm=1" {
		t.Fatalf("expected pending cookie to remember remember-me intent, got %q", pendingCookie.Value)
	}

	code, _ := auth.TOTPCodeAt(secret, time.Now())
	verifyResp := httptest.NewRecorder()
	verifyReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", strings.NewReader(`{"code":"`+code+`"}`))
	verifyReq.Header.Set(requestIntentHeaderName, requestIntentHeaderValueFetch)
	verifyReq.Header.Set("Content-Type", "application/json")
	verifyReq.AddCookie(pendingCookie)
	router.ServeHTTP(verifyResp, verifyReq)

	var sessionCookie *http.Cookie
	for _, cookie := range verifyResp.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected real session cookie after MFA verify")
	}
	if sessionCookie.MaxAge <= 0 {
		t.Fatalf("expected remember-me session cookie to be persistent (MaxAge>0), got MaxAge=%d", sessionCookie.MaxAge)
	}
}

func TestMFARememberMeSetsPersistentCookie(t *testing.T) {
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour, RememberMeTTL: 30 * 24 * time.Hour}
	handler := newMFAHandlerForTest(config, nil)
	router := NewRouter(nil, nil, nil, nil, config, handler, "")

	loginResp := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"secret","rememberMe":true}`))
	loginReq.Header.Set(requestIntentHeaderName, requestIntentHeaderValueFetch)
	loginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(loginResp, loginReq)

	var sessionCookie *http.Cookie
	for _, cookie := range loginResp.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected session cookie")
	}
	if sessionCookie.MaxAge <= 0 {
		t.Fatalf("expected remember-me cookie to be persistent, got MaxAge=%d", sessionCookie.MaxAge)
	}
	// 持久会话有效期应使用配置的 RememberMeTTL（30 天），而非普通会话 TTL。
	if sessionCookie.MaxAge < int((30*24*time.Hour).Seconds())-5 {
		t.Fatalf("expected remember-me cookie MaxAge near 30d, got %d", sessionCookie.MaxAge)
	}
}

func TestMFANonRememberMeSessionUsesStandardTTLCookie(t *testing.T) {
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	handler := newMFAHandlerForTest(config, nil)
	router := NewRouter(nil, nil, nil, nil, config, handler, "")

	loginResp := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"secret"}`))
	loginReq.Header.Set(requestIntentHeaderName, requestIntentHeaderValueFetch)
	loginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(loginResp, loginReq)

	var sessionCookie *http.Cookie
	for _, cookie := range loginResp.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected session cookie")
	}
	// 非 Remember Me 会话保持原语义：Cookie 到期时间与服务端 session TTL 一致。
	expectedMaxAge := int(time.Hour.Seconds())
	if sessionCookie.MaxAge < expectedMaxAge-5 {
		t.Fatalf("expected session cookie MaxAge near %d, got %d", expectedMaxAge, sessionCookie.MaxAge)
	}
}
