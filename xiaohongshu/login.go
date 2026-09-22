package xiaohongshu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/pkg/errors"
)

type LoginAction struct {
	page              *rod.Page
	initialWebSession string
}

func NewLogin(page *rod.Page) *LoginAction {
	return &LoginAction{page: page}
}

var loggedInSelectors = []string{
	`li.user.side-bar-component a[href*="/user/profile/"]`,
	`.side-bar-component a[href*="/user/profile/"]`,
	`a.link-wrapper[href*="/user/profile/"]`,
}

const qrLoginStatusEndpoint = "/api/sns/web/v1/login/qrcode/status"

func (a *LoginAction) CheckLoginStatus(ctx context.Context) (bool, error) {
	// 加超时保护：只是查登录态的快速检查，不应无限挂（登录扫码的等待在 Login/WaitForLogin 里）
	pp := a.page.Context(ctx)
	if err := navigateLoginPage(ctx, pp); err != nil {
		return false, errors.Wrap(err, "navigate to explore failed")
	}
	// Third-party resources occasionally keep the load event pending. The page
	// state and login controls can still be inspected after that timeout.
	_ = pp.Timeout(5 * time.Second).WaitLoad()

	time.Sleep(1 * time.Second)

	return pageReportsLoggedIn(pp)
}

// CurrentUser 当前登录用户的基础信息。
type CurrentUser struct {
	Nickname string `json:"nickname"`
	UserID   string `json:"userId"`
}

// CurrentUser 从当前页面的 __INITIAL_STATE__ 读取登录用户信息。
// 需在 CheckLoginStatus 之后调用：复用已加载的 explore 页，不做额外导航。
func (a *LoginAction) CurrentUser(ctx context.Context) (*CurrentUser, error) {
	pp := a.page.Context(ctx).Timeout(10 * time.Second)

	res, err := pp.Eval(`() => {
		const u = window.__INITIAL_STATE__ && window.__INITIAL_STATE__.user;
		const raw = u && u.userInfo;
		const info = raw && raw.value !== undefined ? raw.value : (raw && raw._value !== undefined ? raw._value : raw);
		if (!info || info.guest || !(info.userId || info.user_id)) return "";
		return JSON.stringify({nickname: info.nickname, userId: info.userId || info.user_id});
	}`)
	if err != nil {
		return nil, errors.Wrap(err, "read current user state failed")
	}

	raw := res.Value.String()
	if raw == "" {
		return nil, errors.New("current user not found in page state")
	}

	var user CurrentUser
	if err := json.Unmarshal([]byte(raw), &user); err != nil {
		return nil, errors.Wrap(err, "unmarshal current user failed")
	}

	return &user, nil
}

func (a *LoginAction) Login(ctx context.Context) error {
	pp := a.page.Context(ctx)

	// 导航到小红书首页，这会触发二维码弹窗
	pp.MustNavigate("https://www.xiaohongshu.com/explore").MustWaitLoad()

	time.Sleep(2 * time.Second)

	if loggedIn, _ := pageReportsLoggedIn(pp); loggedIn {
		return nil
	}

	for !a.WaitForLogin(ctx) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	return nil
}

type qrImageMetadata struct {
	ClassName string `json:"class_name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Visible   bool   `json:"visible"`
}

type qrImageCandidate struct {
	qrImageMetadata
	Source string `json:"source"`
}

const qrImageWaitTimeout = 45 * time.Second

func isQRImageCandidate(meta qrImageMetadata) bool {
	if !meta.Visible || meta.Width < 96 || meta.Height < 96 {
		return false
	}
	ratio := float64(meta.Width) / float64(meta.Height)
	if ratio < 0.85 || ratio > 1.15 {
		return false
	}
	className := strings.ToLower(meta.ClassName)
	return strings.Contains(className, "qrcode") ||
		strings.Contains(className, "qr-code") ||
		(meta.Width <= 512 && meta.Height <= 512)
}

func (a *LoginAction) FetchQrcodeImage(ctx context.Context) (string, bool, error) {
	pp := a.page.Context(ctx)

	// 导航到小红书首页，这会触发二维码弹窗
	if err := navigateLoginPage(ctx, pp); err != nil {
		return "", false, errors.Wrap(err, "navigate to explore failed")
	}
	// Do not fail QR creation just because analytics, images, or another
	// third-party resource prevents the browser's load event from completing.
	// Continue with the original request context and poll the actual QR DOM.
	_ = pp.Timeout(5 * time.Second).WaitLoad()
	if err := ctx.Err(); err != nil {
		return "", false, err
	}

	// 页面和二维码弹窗是异步加载的，不能只固定等待 2 秒。轮询期间同时检查
	// 登录态，避免已登录时把“没有二维码”误报成失败。
	deadline := time.Now().Add(qrImageWaitTimeout)
	startedAt := time.Now()
	openedLoginDialog := false
	for time.Now().Before(deadline) {
		if loggedIn, _ := pageReportsLoggedIn(pp); loggedIn {
			return "", true, nil
		}

		if src, found := findQRCodeImage(pp); found {
			// Record the baseline only after navigation and cookie injection have
			// completed. A later web_session change is therefore scan-driven.
			if session, cookieErr := a.webSessionCookie(); cookieErr == nil {
				a.initialWebSession = session
			}
			return src, false, nil
		}

		// Some explore variants no longer open the login panel automatically.
		// Make one conservative attempt to click an explicitly labelled login
		// control, then keep polling the QR DOM under the session deadline.
		if !openedLoginDialog && time.Since(startedAt) >= 3*time.Second {
			openedLoginDialog = true
			_, _ = pp.Timeout(time.Second).Eval(`() => {
				const candidates = [...document.querySelectorAll('button,a,[role="button"]')];
				const login = candidates.find(el => (el.textContent || '').trim() === '登录');
				if (!login) return false;
				login.click();
				return true;
			}`)
		}

		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}

	return "", false, errors.Errorf("qrcode image not found after waiting %s; the login page may have changed or the request may be blocked", qrImageWaitTimeout)
}

func findQRCodeImage(page *rod.Page) (string, bool) {
	result, err := page.Timeout(time.Second).Eval(`() => {
		const selectors = [
			'.login-container .qrcode-img',
			'.login-container img[src^="data:image"]',
			'img.qrcode-img',
			'img[class*="qrcode"]'
		];
		const seen = new Set();
		for (const selector of selectors) {
			for (const image of document.querySelectorAll(selector)) {
				if (seen.has(image)) continue;
				seen.add(image);
				const rect = image.getBoundingClientRect();
				const source = image.getAttribute('src') || image.getAttribute('data-src') || '';
				if (!source) continue;
				return JSON.stringify({
					class_name: typeof image.className === 'string' ? image.className : '',
					width: image.naturalWidth || Math.round(rect.width),
					height: image.naturalHeight || Math.round(rect.height),
					visible: rect.width > 0 && rect.height > 0 &&
						getComputedStyle(image).visibility !== 'hidden' &&
						getComputedStyle(image).display !== 'none',
					source
				});
			}
		}
		return '';
	}`)
	if err != nil || result.Value.String() == "" {
		return "", false
	}
	var candidate qrImageCandidate
	if json.Unmarshal([]byte(result.Value.String()), &candidate) != nil ||
		!isQRImageCandidate(candidate.qrImageMetadata) || strings.TrimSpace(candidate.Source) == "" {
		return "", false
	}
	return candidate.Source, true
}

func navigateLoginPage(ctx context.Context, page *rod.Page) error {
	err := page.Timeout(30 * time.Second).Navigate("https://www.xiaohongshu.com/explore")
	if err == nil || ignoreLoginNavigationError(ctx, err) {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func ignoreLoginNavigationError(ctx context.Context, err error) bool {
	// A navigation-clone deadline can leave Chromium with a usable document.
	// Continue on the caller-owned page, but never swallow caller cancellation
	// or concrete provider/network failures.
	return ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded)
}
func (a *LoginAction) WaitForLogin(ctx context.Context) bool {
	listenerCtx, cancelListener := context.WithCancel(ctx)
	defer cancelListener()
	pp := a.page.Context(listenerCtx)

	confirmed := make(chan struct{}, 1)
	candidates := make(map[proto.NetworkRequestID]int)
	var candidatesMu sync.Mutex

	if err := (proto.NetworkEnable{}).Call(pp); err == nil {
		waitEvents := pp.EachEvent(
			func(event *proto.NetworkResponseReceived) {
				if event.Response == nil || !strings.Contains(event.Response.URL, qrLoginStatusEndpoint) {
					return
				}
				candidatesMu.Lock()
				candidates[event.RequestID] = event.Response.Status
				candidatesMu.Unlock()
			},
			func(event *proto.NetworkLoadingFinished) {
				candidatesMu.Lock()
				status, ok := candidates[event.RequestID]
				delete(candidates, event.RequestID)
				candidatesMu.Unlock()
				if !ok || status < 200 || status >= 300 {
					return
				}

				requestID := event.RequestID
				go func() {
					body, err := (proto.NetworkGetResponseBody{RequestID: requestID}).Call(pp)
					if err != nil || body == nil {
						return
					}
					raw := []byte(body.Body)
					if body.Base64Encoded {
						raw, err = base64.StdEncoding.DecodeString(body.Body)
						if err != nil {
							return
						}
					}
					if isConfirmedQRLoginResponse(raw) {
						select {
						case confirmed <- struct{}{}:
						default:
						}
					}
				}()
			},
		)
		go waitEvents()
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-confirmed:
			return true
		case <-ticker.C:
			if loggedIn, _ := pageReportsLoggedIn(pp); loggedIn {
				return true
			}

			currentSession, err := a.webSessionCookie()
			if err == nil && currentSession != "" && currentSession != a.initialWebSession {
				return true
			}
		}
	}
}

func pageReportsLoggedIn(page *rod.Page) (bool, error) {
	// __INITIAL_STATE__ is a data contract used by the page itself and is more
	// stable than presentation-only CSS class names.
	result, stateErr := page.Eval(`() => {
		const user = window.__INITIAL_STATE__ && window.__INITIAL_STATE__.user;
		const raw = user && user.userInfo;
		const info = raw && raw.value !== undefined ? raw.value :
			(raw && raw._value !== undefined ? raw._value : raw);
		return Boolean(info && !info.guest &&
			(info.userId || info.user_id));
	}`)
	if stateErr == nil && result.Value.Bool() {
		return true, nil
	}

	var selectorErr error
	for _, selector := range loggedInSelectors {
		exists, _, err := page.Has(selector)
		if err != nil {
			selectorErr = err
			continue
		}
		if exists {
			return true, nil
		}
	}

	if stateErr != nil && selectorErr != nil {
		return false, errors.Wrap(stateErr, "read login state failed")
	}
	return false, nil
}

func (a *LoginAction) webSessionCookie() (string, error) {
	cookies, err := a.page.Browser().GetCookies()
	if err != nil {
		return "", errors.Wrap(err, "read browser cookies failed")
	}
	for _, cookie := range cookies {
		if cookie != nil && isWebSessionCookie(cookie.Name, cookie.Value) {
			return strings.TrimSpace(cookie.Value), nil
		}
	}
	return "", nil
}

func isWebSessionCookie(name, value string) bool {
	return name == "web_session" && strings.TrimSpace(value) != ""
}

func isConfirmedQRLoginResponse(raw []byte) bool {
	var envelope struct {
		Success *bool           `json:"success"`
		Code    int             `json:"code"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return false
	}
	if envelope.Success != nil && !*envelope.Success {
		return false
	}
	if envelope.Code != 0 {
		return false
	}

	payload := json.RawMessage(raw)
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		payload = envelope.Data
	}
	var completion struct {
		Session   string `json:"session"`
		LoginInfo struct {
			Session       string `json:"session"`
			SecureSession string `json:"secure_session"`
		} `json:"login_info"`
	}
	if err := json.Unmarshal(payload, &completion); err != nil {
		return false
	}
	return strings.TrimSpace(completion.Session) != "" ||
		strings.TrimSpace(completion.LoginInfo.Session) != "" ||
		strings.TrimSpace(completion.LoginInfo.SecureSession) != ""
}
