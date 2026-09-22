package xiaohongshu

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// PublishImageContent 发布图文内容
type PublishImageContent struct {
	Title        string
	Content      string
	Tags         []string
	ImagePaths   []string
	ScheduleTime *time.Time // 定时发布时间，nil 表示立即发布
	IsOriginal   bool       // 是否声明原创
	Visibility   string     // 可见范围: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
	Products     []string   // 商品关键词列表，用于绑定带货商品
}

type PublishAction struct {
	page *rod.Page
}

const (
	urlOfPublic = `https://creator.xiaohongshu.com/publish/publish?source=official`

	// contentElemTimeout 查找正文输入框的轮询窗口
	contentElemTimeout = 10 * time.Second
)

func NewPublishImageAction(page *rod.Page) (*PublishAction, error) {

	pp := page.Timeout(300 * time.Second)
	if err := installPublishShadowRootCapture(pp); err != nil {
		return nil, errors.Wrap(err, "install publish component observer")
	}

	if err := pp.Navigate(urlOfPublic); err != nil {
		return nil, errors.Wrap(err, "导航到发布页面失败")
	}

	if err := pp.WaitLoad(); err != nil {
		logrus.Warnf("等待页面加载出现问题: %v，继续尝试", err)
	}
	time.Sleep(2 * time.Second)

	if err := pp.WaitDOMStable(time.Second, 0.1); err != nil {
		logrus.Warnf("等待 DOM 稳定出现问题: %v，继续尝试", err)
	}
	time.Sleep(1 * time.Second)

	if err := mustClickPublishTab(pp, "上传图文"); err != nil {
		logrus.Errorf("点击上传图文 TAB 失败: %v", err)
		return nil, err
	}

	time.Sleep(1 * time.Second)

	return &PublishAction{
		page: pp,
	}, nil
}

const publishShadowRootCaptureScript = `(() => {
  if (window.__xhsGetShadowRoot) return;
  const roots = new WeakMap();
  const original = Element.prototype.attachShadow;
  const wrapped = function(init) {
    const root = Reflect.apply(original, this, [init]);
    roots.set(this, root);
    return root;
  };
  Object.defineProperty(Element.prototype, 'attachShadow', {
    configurable: true,
    enumerable: false,
    writable: true,
    value: wrapped
  });
  Object.defineProperty(window, '__xhsGetShadowRoot', {
    configurable: false,
    enumerable: false,
    value: (element) => roots.get(element) || element.shadowRoot || null
  });
})();`

// installPublishShadowRootCapture runs before the creator page scripts. The
// current xhs-publish-btn uses a closed ShadowRoot, so the real submit button
// is otherwise inaccessible and clicks on the host element are ignored.
func installPublishShadowRootCapture(page *rod.Page) error {
	_, err := page.EvalOnNewDocument(publishShadowRootCaptureScript)
	return err
}

func (p *PublishAction) Publish(ctx context.Context, content PublishImageContent) error {
	if len(content.ImagePaths) == 0 {
		return errors.New("图片不能为空")
	}

	// 重设超时：.Context(ctx) 会替换掉 NewPublishImageAction 里 Timeout(300s) 的 deadline
	page := p.page.Context(ctx).Timeout(300 * time.Second)

	if err := uploadImages(page, content.ImagePaths); err != nil {
		return errors.Wrap(err, "小红书上传图片失败")
	}

	tags := content.Tags
	if len(tags) >= 10 {
		logrus.Warnf("标签数量超过10，截取前10个标签")
		tags = tags[:10]
	}

	logrus.Infof("发布内容: title=%s, images=%v, tags=%v, schedule=%v, original=%v, visibility=%s, products=%v", content.Title, len(content.ImagePaths), tags, content.ScheduleTime, content.IsOriginal, content.Visibility, content.Products)

	if err := submitPublish(ctx, page, content.Title, content.Content, tags, content.ScheduleTime, content.IsOriginal, content.Visibility, content.Products); err != nil {
		return errors.Wrap(err, "小红书发布失败")
	}

	return nil
}

// hasPopCover 当前页面是否还有挡人的浮层。
func hasPopCover(page *rod.Page) bool {
	has, _, err := page.Has("div.d-popover")
	return err == nil && has
}

// dismissPopCover 关掉挡住发布 TAB 的浮层，按 Esc → 点空白 → 摘节点逐级降级。
// 保留摘节点这一步是因为它只要节点还在就必定生效，前两步是否奏效取决于页面
// 自己有没有写对应的处理，无从预判。
func dismissPopCover(page *rod.Page) {
	if err := page.Keyboard.Press(input.Escape); err != nil {
		logrus.Debugf("按 Esc 关闭浮层失败: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // 技术等待：等浮层收起动画
	if !hasPopCover(page) {
		return
	}

	clickEmptyPosition(page)
	time.Sleep(200 * time.Millisecond)
	if !hasPopCover(page) {
		return
	}

	// 前两步都无效，退回摘节点，保证发布能继续。
	has, elem, err := page.Has("div.d-popover")
	if err != nil || !has {
		return
	}
	logrus.Warn("Esc 与点击空白都未能关闭浮层，改为移除该节点")
	if err := elem.Remove(); err != nil {
		logrus.Warnf("移除浮层失败: %v", err)
	}
}

func clickEmptyPosition(page *rod.Page) {
	pt := proto.Point{
		X: float64(380 + rand.Intn(100)),
		Y: float64(20 + rand.Intn(60)),
	}
	// 兜底操作，点不动就算了，不该 panic
	if err := humanize.ClickAt(page, pt); err != nil {
		logrus.Debugf("点击空位置失败: %v", err)
	}
}

func mustClickPublishTab(page *rod.Page, tabname string) error {
	page.MustElement(`div.upload-content`).MustWaitVisible()

	deadline := time.Now().Add(15 * time.Second)
	blockedAtLeastOnce := false

	for time.Now().Before(deadline) {
		tab, blocked, err := getTabElement(page, tabname)
		if err != nil {
			logrus.Warnf("获取发布 TAB 元素失败: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if tab == nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if blocked {
			blockedAtLeastOnce = true
			logrus.Info("发布 TAB 被遮挡，尝试关闭浮层")
			dismissPopCover(page)
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if err := humanize.Click(tab); err != nil {
			logrus.Warnf("点击发布 TAB 失败: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}

		return nil
	}

	// 区分两种失败：找不到 TAB，和找到了但浮层一直关不掉
	if blockedAtLeastOnce {
		return errors.Errorf("发布 TAB %s 一直被浮层遮挡，Esc 与点击空白都未能关闭", tabname)
	}
	return errors.Errorf("没有找到发布 TAB - %s", tabname)
}

func getTabElement(page *rod.Page, tabname string) (*rod.Element, bool, error) {
	elems, err := page.Elements("div.creator-tab")
	if err != nil {
		return nil, false, err
	}

	for _, elem := range elems {
		if !isElementVisible(elem) {
			continue
		}

		text, err := elem.Text()
		if err != nil {
			logrus.Debugf("获取发布 TAB 文本失败: %v", err)
			continue
		}

		if strings.TrimSpace(text) != tabname {
			continue
		}

		blocked, err := isElementBlocked(elem)
		if err != nil {
			return nil, false, err
		}

		return elem, blocked, nil
	}

	return nil, false, nil
}

func isElementBlocked(elem *rod.Element) (bool, error) {
	result, err := elem.Eval(`() => {
		const rect = this.getBoundingClientRect();
		if (rect.width === 0 || rect.height === 0) {
			return true;
		}
		const x = rect.left + rect.width / 2;
		const y = rect.top + rect.height / 2;
		const target = document.elementFromPoint(x, y);
		return !(target === this || this.contains(target));
	}`)
	if err != nil {
		return false, err
	}

	return result.Value.Bool(), nil
}

func uploadImages(page *rod.Page, imagesPaths []string) error {
	validPaths := make([]string, 0, len(imagesPaths))
	for _, path := range imagesPaths {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			logrus.Warnf("图片文件不存在: %s", path)
			continue
		}
		validPaths = append(validPaths, path)
		logrus.Infof("获取有效图片：%s", path)
	}

	// 逐张上传：每张上传后等待预览出现，再上传下一张
	for i, path := range validPaths {
		uploadInput, err := findImageUploadInput(page, i == 0)
		if err != nil {
			return errors.Wrapf(err, "查找上传输入框失败(第%d张)", i+1)
		}
		if err := uploadInput.SetFiles([]string{path}); err != nil {
			return errors.Wrapf(err, "上传第%d张图片失败", i+1)
		}

		slog.Info("图片已提交上传", "index", i+1, "path", path)

		// 等待当前图片上传完成（预览元素数量达到 i+1），最多等 60 秒
		if err := waitForUploadComplete(page, i+1); err != nil {
			return errors.Wrapf(err, "第%d张图片上传超时", i+1)
		}
		time.Sleep(1 * time.Second)
	}

	return nil
}

// findImageUploadInput 查找图片上传的输入框
func findImageUploadInput(page *rod.Page, first bool) (*rod.Element, error) {
	if first {
		return page.Element(".upload-input")
	}

	inputs, err := page.Elements(`input[type="file"]`)
	if err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return nil, errors.New("页面没有文件上传输入框")
	}

	for _, input := range inputs {
		accept, err := input.Attribute("accept")
		if err != nil || accept == nil {
			continue
		}
		if acceptsImage(*accept) {
			return input, nil
		}
	}

	return inputs[0], nil
}

// acceptsImage 判断 accept 属性是否接受图片
func acceptsImage(accept string) bool {
	accept = strings.ToLower(accept)
	if strings.Contains(accept, "image/") {
		return true
	}

	for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp", ".heic"} {
		if strings.Contains(accept, ext) {
			return true
		}
	}
	return false
}

// waitForUploadComplete 等待第 expectedCount 张图片上传完成，最多等 60 秒
func waitForUploadComplete(page *rod.Page, expectedCount int) error {
	maxWaitTime := 60 * time.Second
	checkInterval := 500 * time.Millisecond
	start := time.Now()
	lastLogCount := expectedCount - 1

	for time.Since(start) < maxWaitTime {
		uploadedImages, err := page.Elements(".img-preview-area .pr")
		if err != nil {
			time.Sleep(checkInterval)
			continue
		}

		currentCount := len(uploadedImages)
		if currentCount != lastLogCount {
			slog.Info("等待图片上传", "current", currentCount, "expected", expectedCount)
			lastLogCount = currentCount
		}
		if currentCount >= expectedCount {
			slog.Info("图片上传完成", "count", currentCount)
			return nil
		}

		time.Sleep(checkInterval)
	}

	return errors.Errorf("第%d张图片上传超时(60s)，请检查网络连接和图片大小", expectedCount)
}

func submitPublish(ctx context.Context, page *rod.Page, title, content string, tags []string, scheduleTime *time.Time, isOriginal bool, visibility string, products []string) error {
	titleElem, err := page.Element("div.d-input input")
	if err != nil {
		return errors.Wrap(err, "查找标题输入框失败")
	}
	if err := humanize.Type(ctx, titleElem, title); err != nil {
		return errors.Wrap(err, "输入标题失败")
	}

	humanize.Delay(ctx, humanize.AfterType)
	if err := checkTitleMaxLength(page); err != nil {
		return err
	}
	slog.Info("检查标题长度：通过")

	humanize.Delay(ctx, humanize.AfterType)

	contentElem, err := getContentElement(page, contentElemTimeout)
	if err != nil {
		return err
	}
	if err := humanize.Type(ctx, contentElem, content); err != nil {
		return errors.Wrap(err, "输入正文失败")
	}
	if err := waitAndClickTitleInput(titleElem); err != nil {
		return err
	}
	if err := inputTags(ctx, contentElem, tags); err != nil {
		return err
	}

	humanize.Delay(ctx, humanize.AfterType)

	if err := checkContentMaxLength(page); err != nil {
		return err
	}
	slog.Info("检查正文长度：通过")

	if scheduleTime != nil {
		if err := setSchedulePublish(ctx, page, *scheduleTime); err != nil {
			return errors.Wrap(err, "设置定时发布失败")
		}
		slog.Info("定时发布设置完成", "schedule_time", scheduleTime.Format("2006-01-02 15:04"))
	}

	if err := setVisibility(page, visibility); err != nil {
		return errors.Wrap(err, "设置可见范围失败")
	}

	// 处理原创声明：显式请求了原创但设置失败 → 报错中止，不静默发成非原创（避免"以为原创其实不是"）
	if isOriginal {
		if err := setOriginal(page); err != nil {
			return errors.Wrap(err, "设置原创声明失败（已请求原创，中止发布）")
		}
		slog.Info("已声明原创")
	}

	if err := bindProducts(ctx, page, products); err != nil {
		return errors.Wrap(err, "绑定商品失败")
	}

	if err := clickPublishButton(page); err != nil {
		return err
	}

	// 校验发布真的成功：成功后创作平台会跳转离开发布页；未跳转则判定失败，
	// 消除"点了发布按钮就算成功"的假阳性。
	return waitPublishSuccess(page, 15*time.Second)
}

// waitPublishSuccess 轮询等待发布成功的信号：小红书发布成功后会跳转离开发布表单页
// （URL 不再含 /publish/publish）。超时仍未跳转 → 判定发布失败。
func waitPublishSuccess(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastState publishPageState
	for {
		state, err := readPublishPageState(page)
		if err == nil {
			lastState = state
		}
		if lastState.URL != "" && !strings.Contains(lastState.URL, "/publish/publish") {
			slog.Info("发布成功，已跳转离开发布页", "url", lastState.URL)
			return nil
		}
		if lastState.SuccessFeedback {
			slog.Info("发布成功，已收到平台成功提示")
			return nil
		}
		if time.Now().After(deadline) {
			// The destructive click already happened. This is not a provider
			// rejection: the caller must persist unknown and reconcile read-only.
			diagnosticPath := persistPublishDiagnostic(page, lastState)
			details := lastState.summary()
			if diagnosticPath != "" {
				details += "; diagnostic=" + diagnosticPath
			}
			return errors.Errorf("publish_outcome_unknown: 发布未确认成功：点击发布后未跳转离开发布页; %s", details)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

type publishPageState struct {
	URL             string   `json:"url"`
	SubmitDisabled  string   `json:"submit_disabled,omitempty"`
	SubmitLoading   string   `json:"submit_loading,omitempty"`
	Feedback        []string `json:"feedback,omitempty"`
	ClickTrace      []string `json:"click_trace,omitempty"`
	SuccessFeedback bool     `json:"success_feedback,omitempty"`
	CapturedAt      string   `json:"captured_at,omitempty"`
}

func (s publishPageState) summary() string {
	parts := []string{
		"url=" + s.URL,
		"submit_disabled=" + s.SubmitDisabled,
		"submit_loading=" + s.SubmitLoading,
	}
	if len(s.ClickTrace) > 0 {
		parts = append(parts, "click_trace="+strings.Join(s.ClickTrace, ","))
	}
	if len(s.Feedback) > 0 {
		parts = append(parts, "feedback="+strings.Join(s.Feedback, " | "))
	}
	return strings.Join(parts, "; ")
}

func readPublishPageState(page *rod.Page) (publishPageState, error) {
	result, err := page.Eval(`() => {
    const visible = (el) => {
      if (!el) return false;
      const style = getComputedStyle(el);
      const rect = el.getBoundingClientRect();
      return style.display !== 'none' && style.visibility !== 'hidden' &&
        Number(style.opacity || 1) !== 0 && rect.width > 0 && rect.height > 0;
    };
    const selectors = [
      '[role="alert"]', '[role="dialog"]',
      '.d-message', '.d-toast', '.d-notification',
      '[class*="toast"]', '[class*="message"]', '[class*="error"]',
      '[class*="modal"]:not(.multi-goods-selector-modal)'
    ];
    const feedback = [];
    const seen = new Set();
    for (const el of document.querySelectorAll(selectors.join(','))) {
      if (!visible(el)) continue;
      const text = (el.innerText || el.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 300);
      if (!text || seen.has(text)) continue;
      seen.add(text);
      feedback.push(text);
      if (feedback.length >= 8) break;
    }
    const host = [...document.querySelectorAll('xhs-publish-btn')]
      .find(el => visible(el) && el.getAttribute('is-publish') !== 'false');
    const trace = window.__xhsPublishClickTrace || null;
    return JSON.stringify({
      url: location.href,
      submit_disabled: host ? (host.getAttribute('submit-disabled') || '') : 'host_not_found',
      submit_loading: host ? (host.getAttribute('submit-loading') || '') : 'host_not_found',
      feedback,
      click_trace: trace && Array.isArray(trace.transitions) ? trace.transitions.slice(-12) : [],
      success_feedback: feedback.some(text => /发布成功|已发布/.test(text))
    });
  }`)
	if err != nil {
		return publishPageState{}, err
	}
	var state publishPageState
	if err := json.Unmarshal([]byte(result.Value.Str()), &state); err != nil {
		return publishPageState{}, err
	}
	return state, nil
}

func persistPublishDiagnostic(page *rod.Page, state publishPageState) string {
	dir := strings.TrimSpace(os.Getenv("XHS_DIAGNOSTICS_DIR"))
	if dir == "" {
		if cookiePath := strings.TrimSpace(os.Getenv("COOKIES_PATH")); cookiePath != "" {
			dir = filepath.Join(filepath.Dir(cookiePath), "diagnostics")
		}
	}
	if dir == "" || os.MkdirAll(dir, 0700) != nil {
		return ""
	}

	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	base := filepath.Join(dir, "publish_unknown_"+stamp)
	state.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	metadata, marshalErr := json.MarshalIndent(state, "", "  ")
	if marshalErr == nil {
		_ = os.WriteFile(base+".json", metadata, 0600)
	}
	if screenshot, screenshotErr := page.Screenshot(false, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng}); screenshotErr == nil {
		_ = os.WriteFile(base+".png", screenshot, 0600)
	}
	return base + ".json"
}

type publishButton struct {
	elem     *rod.Element
	isWidget bool
}

func clickPublishButton(page *rod.Page) error {
	btn, err := waitForPublishButtonClickable(page, 15*time.Second)
	if err != nil {
		return err
	}

	if btn.isWidget {
		return clickPublishWidget(page, btn.elem)
	}

	if err := humanize.Click(btn.elem); err != nil {
		return errors.Wrap(err, "点击发布按钮失败")
	}
	return nil
}

// waitForPublishButtonClickable 等待新版 xhs-publish-btn 或旧版 button.bg-red 可点击。
func waitForPublishButtonClickable(page *rod.Page, maxWait time.Duration) (*publishButton, error) {
	interval := 1 * time.Second
	start := time.Now()
	var lastDisabledReason string

	slog.Info("开始等待发布按钮可点击")

	for time.Since(start) < maxWait {
		btn, disabledReason, err := findPublishButton(page)
		if err != nil {
			slog.Warn("查找发布按钮失败，继续等待", "error", err)
			time.Sleep(interval)
			continue
		}
		if btn != nil && disabledReason == "" {
			return btn, nil
		}
		if disabledReason != "" {
			lastDisabledReason = disabledReason
		}
		time.Sleep(interval)
	}

	if lastDisabledReason != "" {
		return nil, errors.Errorf("等待发布按钮可点击超时: %s", lastDisabledReason)
	}
	return nil, errors.New("等待发布按钮可点击超时")
}

func findPublishButton(page *rod.Page) (*publishButton, string, error) {
	widgets, err := page.Elements("xhs-publish-btn")
	if err != nil {
		return nil, "", errors.Wrap(err, "查找新版发布按钮失败")
	}

	for _, widget := range widgets {
		if !isElementVisible(widget) {
			continue
		}

		isPublish, err := widget.Attribute("is-publish")
		if err != nil {
			return nil, "", errors.Wrap(err, "读取新版发布按钮 is-publish 属性失败")
		}
		if isPublish != nil && *isPublish == "false" {
			continue
		}

		submitDisabled, err := widget.Attribute("submit-disabled")
		if err != nil {
			return nil, "", errors.Wrap(err, "读取新版发布按钮 submit-disabled 属性失败")
		}
		if submitDisabled != nil && *submitDisabled == "true" {
			return &publishButton{elem: widget, isWidget: true}, "新版发布按钮不可点击", nil
		}
		submitLoading, err := widget.Attribute("submit-loading")
		if err != nil {
			return nil, "", errors.Wrap(err, "读取新版发布按钮 submit-loading 属性失败")
		}
		if submitLoading != nil && *submitLoading == "true" {
			return &publishButton{elem: widget, isWidget: true}, "新版发布按钮正在提交", nil
		}

		return &publishButton{elem: widget, isWidget: true}, "", nil
	}

	oldButtons, err := page.Elements(".publish-page-publish-btn button.bg-red")
	if err != nil {
		return nil, "", errors.Wrap(err, "查找旧版发布按钮失败")
	}

	for _, oldButton := range oldButtons {
		if !isElementVisible(oldButton) {
			continue
		}

		if disabled, err := oldButton.Attribute("disabled"); err != nil {
			return nil, "", errors.Wrap(err, "读取旧版发布按钮 disabled 属性失败")
		} else if disabled != nil {
			return &publishButton{elem: oldButton}, "旧版发布按钮 disabled", nil
		}

		if ariaDisabled, err := oldButton.Attribute("aria-disabled"); err != nil {
			return nil, "", errors.Wrap(err, "读取旧版发布按钮 aria-disabled 属性失败")
		} else if ariaDisabled != nil && *ariaDisabled == "true" {
			return &publishButton{elem: oldButton}, "旧版发布按钮 aria-disabled=true", nil
		}

		if cls, err := oldButton.Attribute("class"); err != nil {
			return nil, "", errors.Wrap(err, "读取旧版发布按钮 class 属性失败")
		} else if cls != nil && hasExactClass(*cls, "disabled") {
			return &publishButton{elem: oldButton}, "旧版发布按钮包含 disabled class", nil
		}

		return &publishButton{elem: oldButton}, "", nil
	}

	return nil, "", nil
}

func clickPublishWidget(page *rod.Page, widget *rod.Element) error {
	if err := widget.ScrollIntoView(); err != nil {
		return errors.Wrap(err, "滚动新版发布按钮到可视区域失败")
	}
	time.Sleep(200 * time.Millisecond)

	result, err := widget.Eval(`async () => {
    const host = this;
    const getRoot = window.__xhsGetShadowRoot;
    const root = typeof getRoot === 'function' ? getRoot(host) : host.shadowRoot;
    if (!root) {
      return JSON.stringify({clicked: false, reason: 'closed_shadow_root_not_captured'});
    }

    const wanted = (host.getAttribute('submit-text') || '\u53d1\u5e03').trim();
    const normalize = (value) => (value || '').replace(/\s+/g, ' ').trim();
    const all = [...root.querySelectorAll('*')];
    let target = null;
    for (const node of all) {
      const label = normalize(node.innerText || node.textContent ||
        node.getAttribute('aria-label') || node.getAttribute('title'));
      if (label !== wanted) continue;
      target = node.closest('button,[role="button"],input[type="button"],input[type="submit"],.d-button') || node;
      break;
    }
    if (!target) {
      target = root.querySelector(
        'button[type="submit"], [part*="submit"], [class*="submit"] button, button[class*="submit"]'
      );
    }
    if (!target) {
      return JSON.stringify({clicked: false, reason: 'inner_submit_button_not_found'});
    }

    const disabled = !!target.disabled || target.getAttribute('aria-disabled') === 'true' ||
      target.hasAttribute('disabled') || host.getAttribute('submit-disabled') === 'true';
    if (disabled) {
      return JSON.stringify({clicked: false, reason: 'inner_submit_button_disabled'});
    }

    const trace = {transitions: []};
    const record = () => trace.transitions.push(
      'disabled=' + (host.getAttribute('submit-disabled') || '') +
      ',loading=' + (host.getAttribute('submit-loading') || '')
    );
    record();
    const observer = new MutationObserver(record);
    observer.observe(host, {attributes: true, attributeFilter: ['submit-disabled', 'submit-loading']});
    window.__xhsPublishClickTrace = trace;
    setTimeout(() => observer.disconnect(), 30000);

    target.scrollIntoView({block: 'center', inline: 'center'});
    if (typeof target.focus === 'function') target.focus({preventScroll: true});
    // Click the real button inside the captured closed ShadowRoot. Clicking
    // the xhs-publish-btn host or its screen coordinates is ignored.
    HTMLElement.prototype.click.call(target);
    await new Promise(resolve => setTimeout(resolve, 250));
    record();
    return JSON.stringify({
      clicked: true,
      method: 'closed_shadow_inner_button',
      transitions: trace.transitions
    });
  }`)
	if err != nil {
		return errors.Wrap(err, "点击新版发布组件内部按钮失败")
	}
	var clickResult struct {
		Clicked     bool     `json:"clicked"`
		Method      string   `json:"method"`
		Reason      string   `json:"reason"`
		Transitions []string `json:"transitions"`
	}
	if err := json.Unmarshal([]byte(result.Value.Str()), &clickResult); err != nil {
		return errors.Wrap(err, "解析新版发布按钮点击结果失败")
	}
	if !clickResult.Clicked {
		return errors.Errorf("点击新版发布按钮失败（未触发提交）: %s", clickResult.Reason)
	}
	slog.Info("已点击新版发布组件内部真实按钮", "method", clickResult.Method, "trace", clickResult.Transitions)
	return nil
}

// waitAndClickTitleInput 在填写正文后等待 1 秒并回点标题输入框，增强后续交互稳定性
func waitAndClickTitleInput(titleElem *rod.Element) error {
	slog.Info("正文填写完成，准备等待后回点标题输入框")
	time.Sleep(1 * time.Second)
	if err := humanize.ClickWithTimeout(titleElem, 5*time.Second); err != nil {
		slog.Warn("回点标题输入框首次点击失败，尝试降级点击", "error", err)
		if err2 := humanize.ClickNoWait(titleElem); err2 != nil {
			return errors.Wrap(err2, "回点标题输入框失败")
		}
	}
	slog.Info("已回点标题输入框，继续后续发布流程")
	return nil
}

// 检查标题是否超过最大长度
func checkTitleMaxLength(page *rod.Page) error {
	has, elem, err := page.Has(`div.title-container div.max_suffix`)
	if err != nil {
		return errors.Wrap(err, "检查标题长度元素失败")
	}

	if !has {
		return nil
	}

	titleLength, err := elem.Text()
	if err != nil {
		return errors.Wrap(err, "获取标题长度文本失败")
	}

	return makeMaxLengthError(titleLength)
}

func checkContentMaxLength(page *rod.Page) error {
	has, elem, err := page.Has(`div.edit-container div.length-error`)
	if err != nil {
		return errors.Wrap(err, "检查正文长度元素失败")
	}

	if !has {
		return nil
	}

	contentLength, err := elem.Text()
	if err != nil {
		return errors.Wrap(err, "获取正文长度文本失败")
	}

	return makeMaxLengthError(contentLength)
}

func makeMaxLengthError(elemText string) error {
	parts := strings.Split(elemText, "/")
	if len(parts) != 2 {
		return errors.Errorf("长度超过限制: %s", elemText)
	}

	currLen, maxLen := parts[0], parts[1]

	return errors.Errorf("当前输入长度为%s，最大长度为%s", currLen, maxLen)
}

// contentElemSelectors 正文输入框的候选选择器，按先后顺序尝试。
var contentElemSelectors = []string{
	`div[role="textbox"][contenteditable="true"]`,
	`div.tiptap[contenteditable="true"]`,
	`div.ql-editor`,
}

// getContentElement 在 timeout 内轮询查找正文输入框，全部落空返回错误。
func getContentElement(page *rod.Page, timeout time.Duration) (*rod.Element, error) {
	deadline := time.Now().Add(timeout)

	for {
		elem, err := findContentElement(page)
		if err == nil {
			return elem, nil
		}

		if time.Now().After(deadline) {
			return nil, errors.Wrap(err, "查找正文输入框失败")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func findContentElement(page *rod.Page) (*rod.Element, error) {
	for _, selector := range contentElemSelectors {
		elems, err := page.Elements(selector)
		if err != nil {
			return nil, errors.Wrapf(err, "查找正文输入框失败: %s", selector)
		}

		for _, elem := range elems {
			if isElementVisible(elem) {
				return elem, nil
			}
		}
	}

	return findTextboxByPlaceholder(page)
}

func inputTags(ctx context.Context, contentElem *rod.Element, tags []string) error {
	if len(tags) == 0 {
		return nil
	}

	time.Sleep(1 * time.Second)

	for i := 0; i < 20; i++ {
		ka, err := contentElem.KeyActions()
		if err != nil {
			return errors.Wrap(err, "创建键盘操作失败")
		}
		if err := ka.Type(input.ArrowDown).Do(); err != nil {
			return errors.Wrap(err, "按下方向键失败")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ka, err := contentElem.KeyActions()
	if err != nil {
		return errors.Wrap(err, "创建键盘操作失败")
	}
	if err := ka.Press(input.Enter).Press(input.Enter).Do(); err != nil {
		return errors.Wrap(err, "按下回车键失败")
	}

	time.Sleep(1 * time.Second)

	for _, tag := range tags {
		tag = strings.TrimLeft(tag, "#")
		if err := inputTag(ctx, contentElem, tag); err != nil {
			return errors.Wrapf(err, "输入标签[%s]失败", tag)
		}
	}
	return nil
}

func inputTag(ctx context.Context, contentElem *rod.Element, tag string) error {
	// 输入 # 触发话题联想
	if err := humanize.Type(ctx, contentElem, "#"); err != nil {
		return errors.Wrap(err, "输入#失败")
	}
	time.Sleep(200 * time.Millisecond) // 技术等待：等联想下拉框弹出

	if err := humanize.Type(ctx, contentElem, tag); err != nil {
		return errors.Wrap(err, "输入标签内容失败")
	}

	time.Sleep(1 * time.Second) // 技术等待：等联想结果刷新

	page := contentElem.Page()
	topicContainer, err := page.Element("#creator-editor-topic-container")
	if err != nil || topicContainer == nil {
		slog.Warn("未找到标签联想下拉框，直接输入空格", "tag", tag)
		return humanize.Type(ctx, contentElem, " ")
	}

	firstItem, err := topicContainer.Element(".item")
	if err != nil || firstItem == nil {
		slog.Warn("未找到标签联想选项，直接输入空格", "tag", tag)
		return humanize.Type(ctx, contentElem, " ")
	}

	if err := humanize.Click(firstItem); err != nil {
		return errors.Wrap(err, "点击标签联想选项失败")
	}
	slog.Info("成功点击标签联想选项", "tag", tag)
	time.Sleep(500 * time.Millisecond) // 技术等待：等标签处理完成
	return nil
}

func findTextboxByPlaceholder(page *rod.Page) (*rod.Element, error) {
	elements, err := page.Elements("p")
	if err != nil {
		return nil, errors.Wrap(err, "查找正文候选元素失败")
	}
	if len(elements) == 0 {
		return nil, errors.New("no p elements found")
	}

	placeholderElem := findPlaceholderElement(elements, "输入正文描述")
	if placeholderElem == nil {
		return nil, errors.New("no placeholder element found")
	}

	textboxElem := findTextboxParent(placeholderElem)
	if textboxElem == nil {
		return nil, errors.New("no textbox parent found")
	}

	return textboxElem, nil
}

func findPlaceholderElement(elements []*rod.Element, searchText string) *rod.Element {
	for _, elem := range elements {
		placeholder, err := elem.Attribute("data-placeholder")
		if err != nil || placeholder == nil {
			continue
		}

		if strings.Contains(*placeholder, searchText) && isElementVisible(elem) {
			return elem
		}
	}
	return nil
}

func findTextboxParent(elem *rod.Element) *rod.Element {
	currentElem := elem
	for i := 0; i < 5; i++ {
		parent, err := currentElem.Parent()
		if err != nil {
			break
		}

		role, err := parent.Attribute("role")
		if err != nil || role == nil {
			currentElem = parent
			continue
		}

		if *role == "textbox" {
			return parent
		}

		currentElem = parent
	}
	return nil
}

// isElementVisible 检查元素是否可见
func isElementVisible(elem *rod.Element) bool {

	style, err := elem.Attribute("style")
	if err == nil && style != nil {
		styleStr := *style

		if strings.Contains(styleStr, "left: -9999px") ||
			strings.Contains(styleStr, "top: -9999px") ||
			strings.Contains(styleStr, "position: absolute; left: -9999px") ||
			strings.Contains(styleStr, "display: none") ||
			strings.Contains(styleStr, "visibility: hidden") ||
			strings.Contains(styleStr, "opacity: 1e-05") {
			return false
		}

		// 精确匹配 opacity: 0（不匹配 0.5、0.1 等）
		if strings.Contains(styleStr, "opacity: 0") {
			// 确认是 opacity: 0 而非 opacity: 0.x
			if matched, _ := regexp.MatchString(`opacity:\s*0(\s|;|$)`, styleStr); matched {
				return false
			}
		}
	}

	ariaHidden, err := elem.Attribute("aria-hidden")
	if err == nil && ariaHidden != nil && *ariaHidden == "true" {
		return false
	}

	// 检查 tabindex 属性（-1 表示不可聚焦，通常也意味着不可见）
	tabindex, err := elem.Attribute("tabindex")
	if err == nil && tabindex != nil && *tabindex == "-1" {
		// 结合检查是否有 active class 来判断是否是真正的隐藏
		class, _ := elem.Attribute("class")
		// 使用单词边界检查，避免匹配 "inactive" 等
		if class == nil || !hasExactClass(*class, "active") {
			// 不是激活状态的 -1 tabindex 元素，可能是隐藏的叠加层
			return false
		}
	}

	visible, err := elem.Visible()
	if err != nil {
		slog.Warn("无法获取元素可见性", "error", err)
		return true
	}

	return visible
}

// hasExactClass 检查 class 字符串是否包含指定的完整类名（单词边界匹配）
func hasExactClass(classStr, className string) bool {
	pattern := `\b` + regexp.QuoteMeta(className) + `\b`
	matched, _ := regexp.MatchString(pattern, classStr)
	return matched
}

// setVisibility 设置可见范围
// 支持: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
func setVisibility(page *rod.Page, visibility string) error {
	if visibility == "" || visibility == "公开可见" {
		slog.Info("可见范围使用默认：公开可见")
		return nil
	}

	supported := map[string]bool{"仅自己可见": true, "仅互关好友可见": true}
	if !supported[visibility] {
		return errors.Errorf("不支持的可见范围: %s，支持: 公开可见、仅自己可见、仅互关好友可见", visibility)
	}

	dropdown, err := page.Element("div.permission-card-wrapper div.d-select-content")
	if err != nil {
		return errors.Wrap(err, "查找可见范围下拉框失败")
	}
	if err := humanize.Click(dropdown); err != nil {
		return errors.Wrap(err, "点击可见范围下拉框失败")
	}
	time.Sleep(500 * time.Millisecond)

	// 在弹窗中查找并点击目标选项
	opts, err := page.Elements("div.d-options-wrapper div.d-grid-item div.custom-option")
	if err != nil {
		return errors.Wrap(err, "查找可见范围选项失败")
	}
	for _, opt := range opts {
		text, err := opt.Text()
		if err != nil {
			continue
		}
		if strings.Contains(text, visibility) {
			if err := humanize.Click(opt); err != nil {
				return errors.Wrap(err, "选择可见范围失败")
			}
			slog.Info("已设置可见范围", "visibility", visibility)
			time.Sleep(200 * time.Millisecond)
			return nil
		}
	}
	return errors.Errorf("未找到可见范围选项: %s", visibility)
}

// setSchedulePublish 设置定时发布时间
func setSchedulePublish(ctx context.Context, page *rod.Page, t time.Time) error {
	// 1. 点击定时发布开关
	if err := clickScheduleSwitch(page); err != nil {
		return err
	}
	time.Sleep(800 * time.Millisecond)

	// 2. 设置日期时间
	if err := setDateTime(ctx, page, t); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)

	return nil
}

// clickScheduleSwitch 点击定时发布开关
func clickScheduleSwitch(page *rod.Page) error {
	switchElem, err := page.Element(".post-time-wrapper .d-switch")
	if err != nil {
		return errors.Wrap(err, "查找定时发布开关失败")
	}

	if err := humanize.Click(switchElem); err != nil {
		return errors.Wrap(err, "点击定时发布开关失败")
	}
	slog.Info("已点击定时发布开关")
	return nil
}

// setDateTime 设置日期时间
func setDateTime(ctx context.Context, page *rod.Page, t time.Time) error {
	dateTimeStr := t.Format("2006-01-02 15:04")

	elem, err := page.Element(".date-picker-container input")
	if err != nil {
		return errors.Wrap(err, "查找日期时间输入框失败")
	}

	// SelectAllText 走 Eval(this.select())，只改选区、不额外派发事件，暂时保留。
	// 换成键盘全选要区分 Ctrl/Cmd，换成三击又可能触发日期控件的其他行为。
	if err := elem.SelectAllText(); err != nil {
		return errors.Wrap(err, "选择日期时间文本失败")
	}
	if err := humanize.Type(ctx, elem, dateTimeStr); err != nil {
		return errors.Wrap(err, "输入日期时间失败")
	}
	slog.Info("已设置日期时间", "datetime", dateTimeStr)

	return nil
}

// setOriginal 设置原创声明
func setOriginal(page *rod.Page) error {
	// 根据小红书创作者页面的实际结构：
	// div.custom-switch-card 包含 span.has-tips 文本为"原创声明"
	// 开关是 div.d-switch 组件

	// 查找包含"原创声明"文本的 custom-switch-card
	switchCards, err := page.Elements("div.custom-switch-card")
	if err != nil {
		return errors.Wrap(err, "查找原创声明卡片失败")
	}

	for _, card := range switchCards {
		text, err := card.Text()
		if err != nil {
			continue
		}

		// 检查是否是原创声明卡片
		if !strings.Contains(text, "原创声明") {
			continue
		}

		// 找到原创声明卡片，查找其中的 d-switch
		switchElem, err := card.Element("div.d-switch")
		if err != nil {
			continue
		}

		// 检查开关是否已打开
		checked, err := switchElem.Eval(`() => {
			const input = this.querySelector('input[type="checkbox"]');
			return input ? input.checked : false;
		}`)
		if err != nil {
			continue
		}

		if checked.Value.Bool() {
			slog.Info("原创声明已开启")
			return nil
		}

		// 点击开关
		if err := humanize.Click(switchElem); err != nil {
			return errors.Wrap(err, "点击原创声明开关失败")
		}

		time.Sleep(500 * time.Millisecond)

		// 处理原创声明确认弹窗
		if err := confirmOriginalDeclaration(page); err != nil {
			return errors.Wrap(err, "确认原创声明失败")
		}

		slog.Info("已开启原创声明")
		return nil
	}

	return errors.New("未找到原创声明选项")
}

// confirmOriginalDeclaration 交互（勾选须知、点声明按钮）走 go-rod 点击；
// 仅用只读 Eval 读取 checkbox 勾选态（不产生交互，无法用属性判断的自定义组件才用）。
func confirmOriginalDeclaration(page *rod.Page) error {
	time.Sleep(800 * time.Millisecond) // 技术等待：等确认弹窗渲染

	if footer, err := findFooterByText(page, "原创声明须知"); err != nil {
		slog.Warn("未找到原创声明确认弹窗的 footer", "error", err)
	} else if err := checkFooterCheckbox(footer); err != nil {
		slog.Warn("勾选原创声明须知失败", "error", err)
	}

	time.Sleep(500 * time.Millisecond) // 技术等待：勾选后等"声明原创"按钮变可用

	footer, err := findFooterByText(page, "声明原创")
	if err != nil {
		return errors.Wrap(err, "未找到声明原创弹窗")
	}

	btn, err := footer.Element("button.custom-button")
	if err != nil {
		return errors.Wrap(err, "未找到声明原创按钮")
	}

	if isButtonDisabled(btn) {
		// 兜底：按钮仍禁用，可能须知未勾上，再勾一次
		if err := checkFooterCheckbox(footer); err != nil {
			slog.Warn("二次勾选须知失败", "error", err)
		}
		time.Sleep(300 * time.Millisecond)
		if isButtonDisabled(btn) {
			return errors.New("声明原创按钮仍处于禁用状态")
		}
	}

	if err := humanize.Click(btn); err != nil {
		return errors.Wrap(err, "点击声明原创按钮失败")
	}
	slog.Info("已成功点击声明原创按钮")
	time.Sleep(300 * time.Millisecond)
	return nil
}

func findFooterByText(page *rod.Page, keyword string) (*rod.Element, error) {
	footers, err := page.Elements("div.footer")
	if err != nil {
		return nil, errors.Wrap(err, "查找弹窗 footer 失败")
	}
	for _, footer := range footers {
		text, err := footer.Text()
		if err != nil {
			continue
		}
		if strings.Contains(text, keyword) {
			return footer, nil
		}
	}
	return nil, errors.Errorf("未找到包含%q的弹窗 footer", keyword)
}

// checkFooterCheckbox 勾选 footer 内的自定义 checkbox（未勾选时才点）。
func checkFooterCheckbox(footer *rod.Element) error {
	cb, err := footer.Element("div.d-checkbox")
	if err != nil {
		return errors.Wrap(err, "未找到须知 checkbox")
	}

	// 只读判断当前是否已勾选（隐藏 input.checked 或 simulator 上的 checked 态）
	checked, err := cb.Eval(`() => {
		const input = this.querySelector('input[type="checkbox"]');
		return (input && input.checked) || this.querySelector('.checked') !== null;
	}`)
	if err != nil {
		return errors.Wrap(err, "读取 checkbox 状态失败")
	}
	if checked.Value.Bool() {
		return nil
	}

	return humanize.Click(cb)
}

func isButtonDisabled(btn *rod.Element) bool {
	if disabled, _ := btn.Attribute("disabled"); disabled != nil {
		return true
	}
	if cls, _ := btn.Attribute("class"); cls != nil && hasExactClass(*cls, "disabled") {
		return true
	}
	return false
}

// bindProducts 绑定商品到发布内容
func bindProducts(ctx context.Context, page *rod.Page, products []string) error {
	if len(products) == 0 {
		return nil
	}

	slog.Info("开始绑定商品", "products", products)

	// 点击"添加商品"按钮
	if err := clickAddProductButton(page); err != nil {
		return errors.Wrap(err, "点击添加商品按钮失败")
	}
	time.Sleep(1 * time.Second)

	// 等待商品选择弹窗出现
	modal, err := waitForProductModal(page)
	if err != nil {
		return errors.Wrap(err, "等待商品弹窗失败")
	}
	slog.Info("商品选择弹窗已打开")

	// 遍历搜索并选择商品
	var failedProducts []string
	for _, keyword := range products {
		if err := searchAndSelectProduct(ctx, page, modal, keyword); err != nil {
			slog.Warn("搜索选择商品失败", "keyword", keyword, "error", err)
			failedProducts = append(failedProducts, keyword)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// 点击保存按钮
	slog.Info("准备点击保存按钮")
	if err := clickModalSaveButton(modal); err != nil {
		return errors.Wrap(err, "点击保存按钮失败")
	}
	slog.Info("保存按钮点击完成，开始等待弹窗关闭")

	// 等待弹窗关闭
	if err := waitForModalClose(page); err != nil {
		slog.Warn("等待弹窗关闭超时", "error", err)
	} else {
		slog.Info("弹窗已关闭")
	}

	if len(failedProducts) > 0 {
		return errors.Errorf("部分商品未找到: %v", failedProducts)
	}

	slog.Info("商品绑定完成", "total", len(products))
	time.Sleep(1000 * time.Millisecond)
	return nil
}

// clickAddProductButton 点击"添加商品"按钮
func clickAddProductButton(page *rod.Page) error {
	slog.Info("开始查找添加商品按钮")

	// 查找包含"添加商品"文本的元素
	spans, err := page.Elements("span.d-text")
	if err != nil {
		return errors.Wrap(err, "查找商品按钮文本失败")
	}

	for _, span := range spans {
		text, err := span.Text()
		if err != nil {
			continue
		}
		if strings.TrimSpace(text) == "添加商品" {
			slog.Info("找到添加商品文本，向上查找可点击父元素")
			// 向上查找可点击的父元素
			parent := span
			for i := 0; i < 5; i++ {
				p, err := parent.Parent()
				if err != nil {
					break
				}
				parent = p

				tagName, err := parent.Eval(`() => this.tagName.toLowerCase()`)
				if err != nil {
					continue
				}
				tag := tagName.Value.Str()

				// 检查是否为 button 或含 d-button class
				if tag == "button" {
					if err := humanize.Click(parent); err != nil {
						return errors.Wrap(err, "点击添加商品按钮失败")
					}
					slog.Info("已点击添加商品按钮")
					time.Sleep(300 * time.Millisecond) // 确保弹窗动画开始
					return nil
				}

				cls, _ := parent.Attribute("class")
				if cls != nil && strings.Contains(*cls, "d-button") {
					if err := humanize.Click(parent); err != nil {
						return errors.Wrap(err, "点击添加商品按钮失败")
					}
					slog.Info("已点击添加商品按钮")
					time.Sleep(300 * time.Millisecond) // 确保弹窗动画开始
					return nil
				}
			}
		}
	}

	return errors.New("未找到添加商品按钮，账号可能未开通商品功能")
}

// waitForProductModal 等待商品选择弹窗出现
func waitForProductModal(page *rod.Page) (*rod.Element, error) {
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		modal, err := page.Element(".multi-goods-selector-modal")
		if err == nil && modal != nil {
			visible, _ := modal.Visible()
			if visible {
				slog.Info("商品选择弹窗已出现")
				return modal, nil
			}
		}
		time.Sleep(100 * time.Millisecond) // 缩短轮询间隔，更快响应
	}

	return nil, errors.New("等待商品选择弹窗超时")
}

// searchAndSelectProduct 搜索并选择商品
func searchAndSelectProduct(ctx context.Context, page *rod.Page, modal *rod.Element, keyword string) error {
	slog.Info("搜索商品", "keyword", keyword)

	// 1. 获取搜索框
	searchInput, err := modal.Element(`input[placeholder="搜索商品ID 或 商品名称"]`)
	if err != nil {
		return errors.Wrap(err, "未找到商品搜索框")
	}

	// 2. 清空并输入关键词。SelectAllText 走 Eval(this.select())，只改选区、不额外
	// 派发事件，暂时保留（换键盘全选要区分 Ctrl/Cmd）。
	if err := searchInput.SelectAllText(); err != nil {
		slog.Warn("选择搜索框文本失败", "error", err)
	}
	time.Sleep(100 * time.Millisecond)

	if err := humanize.Type(ctx, searchInput, keyword); err != nil {
		return errors.Wrap(err, "输入搜索关键词失败")
	}
	time.Sleep(300 * time.Millisecond)

	// 3. 触发搜索（模拟键盘 Enter）
	if err := page.Keyboard.Press(input.Enter); err != nil {
		return errors.Wrap(err, "触发搜索失败")
	}

	// 4. 等待搜索结果加载
	time.Sleep(1 * time.Second)

	// 等待 loading 消失（使用与工作代码相同的选择器）
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		loading, err := modal.Element(".goods-list-loading")
		if err != nil || loading == nil {
			break
		}
		visible, _ := loading.Visible()
		if !visible {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 等待商品列表渲染完成（使用与工作代码相同的选择器）
	for time.Now().Before(deadline) {
		productList, err := modal.Element(".goods-list-normal .good-card-container")
		if err == nil && productList != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // 额外等待确保渲染完成

	// 5. 点击第一个商品的 checkbox（使用与工作代码相同的选择器）
	checkbox, err := modal.Element(".goods-list-normal .good-card-container .d-checkbox")
	if err != nil {
		return errors.Wrap(err, "未找到商品选择框")
	}

	// 检查是否已经选中
	isChecked, err := checkbox.Eval(`(el) => {
		return el.querySelector('.d-checkbox-simulator.checked') !== null ||
			   el.querySelector('input[type="checkbox"]:checked') !== null;
	}`)
	if err == nil && isChecked.Value.Bool() {
		slog.Info("商品已选中，跳过", "keyword", keyword)
		return nil
	}

	if err := humanize.Click(checkbox); err != nil {
		return errors.Wrap(err, "点击商品选择框失败")
	}

	randomDelay := 800 + rand.Intn(700)
	time.Sleep(time.Duration(randomDelay) * time.Millisecond)

	slog.Info("已选择商品", "keyword", keyword)
	return nil
}

// clickModalSaveButton 点击保存按钮
func clickModalSaveButton(modal *rod.Element) error {
	// 查找保存按钮（参考工作代码：直接查找并点击，不强制要求找到）
	btn, err := modal.Element(".goods-selected-footer button")
	if err == nil && btn != nil {
		if err := humanize.Click(btn); err != nil {
			slog.Warn("点击保存按钮失败", "error", err)
		} else {
			slog.Info("已点击保存按钮")
			return nil
		}
	}

	// 尝试点击主按钮
	primaryBtn, err := modal.Element(".goods-selected-footer .d-button--primary")
	if err == nil && primaryBtn != nil {
		if err := humanize.Click(primaryBtn); err != nil {
			slog.Warn("点击主按钮失败", "error", err)
		} else {
			slog.Info("已点击主按钮")
			return nil
		}
	}

	slog.Warn("未找到保存按钮，继续执行")
	return nil
}

// waitForModalClose 等待弹窗关闭
func waitForModalClose(page *rod.Page) error {
	deadline := time.Now().Add(5 * time.Second)
	slog.Info("开始等待弹窗关闭")

	for time.Now().Before(deadline) {
		// 使用 Has 代替 Element，避免等待元素出现的阻塞
		has, _, err := page.Has(".multi-goods-selector-modal")
		if err != nil || !has {
			slog.Info("弹窗已关闭")
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	return errors.New("等待弹窗关闭超时")
}
