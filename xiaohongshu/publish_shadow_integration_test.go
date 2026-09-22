//go:build integration

package xiaohongshu

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/stretchr/testify/require"
	appbrowser "github.com/xpzouying/xiaohongshu-mcp/browser"
)

func TestClickPublishWidgetUsesRealButtonInsideClosedShadowRoot(t *testing.T) {
	bin, err := appbrowser.EnsureBrowser()
	if err != nil {
		t.Skipf("browser unavailable: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><html><body>
<script>
window.__publishClicks = 0;
customElements.define('xhs-publish-btn', class extends HTMLElement {
  constructor() {
    super();
    const root = this.attachShadow({mode: 'closed'});
    const save = document.createElement('button');
    save.textContent = '暂存离开';
    const submit = document.createElement('button');
    submit.textContent = '发布';
    submit.addEventListener('click', () => {
      window.__publishClicks++;
      this.setAttribute('submit-loading', 'true');
    });
    root.append(save, submit);
  }
});
</script>
<xhs-publish-btn is-publish="true" submit-text="发布" submit-disabled="false" submit-loading="false"
 style="display:block;width:320px;height:80px"></xhs-publish-btn>
</body></html>`)
	}))
	defer server.Close()

	controlURL := launcher.New().Bin(bin).Headless(true).MustLaunch()
	browser := rod.New().ControlURL(controlURL).MustConnect()
	defer browser.MustClose()
	page := browser.MustPage("about:blank")
	defer page.MustClose()
	require.NoError(t, installPublishShadowRootCapture(page))
	require.NoError(t, page.Navigate(server.URL))
	require.NoError(t, page.WaitLoad())

	widget, err := page.Element("xhs-publish-btn")
	require.NoError(t, err)
	require.NoError(t, clickPublishWidget(page, widget))
	require.Equal(t, 1, page.MustEval(`() => window.__publishClicks`).Int())

	state, err := readPublishPageState(page)
	require.NoError(t, err)
	require.Equal(t, "true", state.SubmitLoading)
	require.NotEmpty(t, state.ClickTrace)
}
