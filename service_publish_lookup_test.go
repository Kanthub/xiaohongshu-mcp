package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

func TestBestEffortProfileLookupConvertsPanicToError(t *testing.T) {
	profile, err := bestEffortProfileLookup(context.Background(), time.Second, func(context.Context) (*UserProfileResponse, error) {
		panic(context.DeadlineExceeded)
	})

	require.Error(t, err)
	assert.Nil(t, profile)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestBestEffortProfileLookupReturnsOrdinaryError(t *testing.T) {
	want := errors.New("profile unavailable")
	profile, err := bestEffortProfileLookup(context.Background(), time.Second, func(context.Context) (*UserProfileResponse, error) {
		return nil, want
	})

	assert.Nil(t, profile)
	assert.ErrorIs(t, err, want)
}

func TestFindPublishedFeed(t *testing.T) {
	feeds := []xiaohongshu.Feed{
		{ID: "incomplete", NoteCard: xiaohongshu.NoteCard{DisplayTitle: "刚发布的笔记"}},
		{ID: "newest", XsecToken: "token-new", NoteCard: xiaohongshu.NoteCard{DisplayTitle: "刚发布的笔记"}},
		{ID: "older", XsecToken: "token-old", NoteCard: xiaohongshu.NoteCard{DisplayTitle: "刚发布的笔记"}},
	}

	feed, ok := findPublishedFeed(feeds, "刚发布的笔记", nil)
	require.True(t, ok)
	assert.Equal(t, "newest", feed.ID)
	assert.Equal(t, "token-new", feed.XsecToken)
}

func TestFindPublishedFeedNotFound(t *testing.T) {
	feeds := []xiaohongshu.Feed{
		{ID: "other", XsecToken: "token", NoteCard: xiaohongshu.NoteCard{DisplayTitle: "其他笔记"}},
	}

	_, ok := findPublishedFeed(feeds, "刚发布的笔记", nil)
	assert.False(t, ok)
}

func TestFindPublishedFeedPrefersNewFeedWhenTitleChanged(t *testing.T) {
	feeds := []xiaohongshu.Feed{
		{ID: "newest", XsecToken: "token-new", NoteCard: xiaohongshu.NoteCard{DisplayTitle: "平台清洗后的标题"}},
		{ID: "old", XsecToken: "token-old", NoteCard: xiaohongshu.NoteCard{DisplayTitle: "旧笔记"}},
	}
	feed, ok := findPublishedFeed(feeds, "原始标题", map[string]struct{}{"old": {}})
	require.True(t, ok)
	assert.Equal(t, "newest", feed.ID)
}

func TestFindPublishedFeedSupportsFirstPostWithChangedTitle(t *testing.T) {
	feeds := []xiaohongshu.Feed{
		{ID: "first", XsecToken: "token-first", NoteCard: xiaohongshu.NoteCard{DisplayTitle: "平台清洗后的标题"}},
	}
	feed, ok := findPublishedFeed(feeds, "原始标题", map[string]struct{}{})
	require.True(t, ok)
	assert.Equal(t, "first", feed.ID)
}

func TestFindPublishedFeedDoesNotReturnOldDuplicateTitle(t *testing.T) {
	feeds := []xiaohongshu.Feed{
		{ID: "new", XsecToken: "token-new", NoteCard: xiaohongshu.NoteCard{DisplayTitle: "平台改写后的标题"}},
		{ID: "old", XsecToken: "token-old", NoteCard: xiaohongshu.NoteCard{DisplayTitle: "原始标题"}},
	}
	feed, ok := findPublishedFeed(feeds, "原始标题", map[string]struct{}{"old": {}})
	require.True(t, ok)
	assert.Equal(t, "new", feed.ID)
}
