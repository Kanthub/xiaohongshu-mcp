package xiaohongshu

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

type NavigateAction struct {
	page *rod.Page
}

func NewNavigate(page *rod.Page) *NavigateAction {
	return &NavigateAction{page: page}
}

func (n *NavigateAction) ToExplorePage(ctx context.Context) error {
	page := n.page.Context(ctx).Timeout(60 * time.Second) // 加超时保护，避免 MustNavigate/MustWaitStable 无限挂

	page.MustNavigate("https://www.xiaohongshu.com/explore").
		MustWaitLoad().
		MustElement(`div#app`)

	return nil
}

func (n *NavigateAction) ToProfilePage(ctx context.Context) error {
	page := n.page.Context(ctx).Timeout(60 * time.Second) // 加超时保护，避免 MustNavigate/MustWaitStable 无限挂

	// First navigate to explore page
	if err := n.ToExplorePage(ctx); err != nil {
		return err
	}

	page.MustWaitStable()

	// 优先读取“我”的链接。只依赖 href，不再依赖 span.channel 等易变的 DOM 层级。
	quickPage := page.Timeout(2 * time.Second)
	selectors := []string{
		`li.user.side-bar-component a[href*="/user/profile/"]`,
		`.side-bar-component a[href*="/user/profile/"]`,
		`a.link-wrapper[href*="/user/profile/"]`,
	}
	for _, selector := range selectors {
		link, err := quickPage.Element(selector)
		if err != nil || link == nil {
			continue
		}
		href, err := link.Attribute("href")
		if err == nil && href != nil && strings.Contains(*href, "/user/profile/") {
			profileURL := *href
			if strings.HasPrefix(profileURL, "/") {
				profileURL = "https://www.xiaohongshu.com" + profileURL
			}
			if err := page.Navigate(profileURL); err != nil {
				return fmt.Errorf("navigate to current profile: %w", err)
			}
			return page.WaitLoad()
		}
	}

	// 页面改版后可能不再渲染旧侧边栏，但登录用户 ID 仍在初始状态里。
	// 用它直达自己的主页，避免为了点击“我”而被过时选择器卡满 60 秒。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res, err := page.Eval(`() => {
			const u = window.__INITIAL_STATE__ && window.__INITIAL_STATE__.user;
			const raw = u && u.userInfo;
			const info = raw && raw.value !== undefined ? raw.value : (raw && raw._value !== undefined ? raw._value : raw);
			return info && !info.guest ? (info.userId || info.user_id || "") : "";
		}`)
		if err == nil {
			if userID := strings.TrimSpace(res.Value.Str()); userID != "" {
				profileURL := "https://www.xiaohongshu.com/user/profile/" + url.PathEscape(userID)
				if err := page.Navigate(profileURL); err != nil {
					return fmt.Errorf("navigate to current profile: %w", err)
				}
				return page.WaitLoad()
			}
		}
		time.Sleep(300 * time.Millisecond)
	}

	return fmt.Errorf("未找到当前账号主页入口，请确认登录状态是否有效")
}
