package xiaohongshu

import (
	"context"
	"errors"
	"testing"
)

func TestIsWebSessionCookie(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "web_session", value: "authenticated", want: true},
		{name: "web_session", value: "  authenticated  ", want: true},
		{name: "web_session", value: "", want: false},
		{name: "web_session", value: "   ", want: false},
		{name: "a1", value: "authenticated", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/"+tt.value, func(t *testing.T) {
			if got := isWebSessionCookie(tt.name, tt.value); got != tt.want {
				t.Fatalf("isWebSessionCookie(%q, %q) = %v, want %v", tt.name, tt.value, got, tt.want)
			}
		})
	}
}

func TestIsConfirmedQRLoginResponse(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "confirmed envelope",
			raw:  `{"success":true,"code":0,"data":{"login_info":{"session":"authenticated"}}}`,
			want: true,
		},
		{
			name: "direct secure session",
			raw:  `{"code":0,"login_info":{"secure_session":"secure"}}`,
			want: true,
		},
		{
			name: "top level session",
			raw:  `{"code":0,"session":"authenticated"}`,
			want: true,
		},
		{name: "waiting", raw: `{"success":true,"code":0,"data":{"codeStatus":0}}`, want: false},
		{name: "scanned", raw: `{"success":true,"code":0,"data":{"codeStatus":1}}`, want: false},
		{name: "failed envelope", raw: `{"success":false,"code":0,"data":{"login_info":{"session":"x"}}}`, want: false},
		{name: "error code", raw: `{"success":true,"code":-1,"data":{"login_info":{"session":"x"}}}`, want: false},
		{name: "invalid JSON", raw: `{`, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isConfirmedQRLoginResponse([]byte(test.raw)); got != test.want {
				t.Fatalf("isConfirmedQRLoginResponse(%s) = %v, want %v", test.raw, got, test.want)
			}
		})
	}
}

func TestLoginNavigationTimeoutPolicy(t *testing.T) {
	active := context.Background()
	if !ignoreLoginNavigationError(active, context.DeadlineExceeded) {
		t.Fatal("an internal navigation deadline should fall through to DOM inspection")
	}
	if ignoreLoginNavigationError(active, errors.New("net::ERR_NAME_NOT_RESOLVED")) {
		t.Fatal("a provider navigation failure must remain fatal")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if ignoreLoginNavigationError(cancelled, context.DeadlineExceeded) {
		t.Fatal("caller cancellation must remain fatal")
	}
}

func TestQRImageCandidateRejectsLogoAndHiddenImages(t *testing.T) {
	tests := []struct {
		name string
		meta qrImageMetadata
		want bool
	}{
		{name: "real qrcode class", meta: qrImageMetadata{ClassName: "qrcode-img", Width: 128, Height: 128, Visible: true}, want: true},
		{name: "square fallback", meta: qrImageMetadata{ClassName: "login-code", Width: 160, Height: 160, Visible: true}, want: true},
		{name: "observed xiaohongshu logo", meta: qrImageMetadata{ClassName: "logo", Width: 205, Height: 96, Visible: true}, want: false},
		{name: "tiny icon", meta: qrImageMetadata{ClassName: "qrcode-icon", Width: 32, Height: 32, Visible: true}, want: false},
		{name: "hidden stale qr", meta: qrImageMetadata{ClassName: "qrcode-img", Width: 128, Height: 128, Visible: false}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isQRImageCandidate(test.meta); got != test.want {
				t.Fatalf("isQRImageCandidate(%#v) = %v, want %v", test.meta, got, test.want)
			}
		})
	}
}
