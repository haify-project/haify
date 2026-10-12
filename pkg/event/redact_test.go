package event

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactURL(t *testing.T) {
	assert.Equal(t, "https://oapi.dingtalk.com", RedactURL("https://oapi.dingtalk.com/robot/send?access_token=abc123"))
	assert.Equal(t, "https://hooks.slack.com", RedactURL("https://hooks.slack.com/services/T0/B0/XXXX"))
	assert.Equal(t, "<url>", RedactURL("not a url"))
}

func TestRedactText(t *testing.T) {
	got := RedactText(`parse "https://open.feishu.cn/open-apis/bot/v2/hook/SECRET": invalid port`)
	assert.Equal(t, `parse "https://open.feishu.cn": invalid port`, got)
	assert.NotContains(t, got, "SECRET")
}

func TestRedactRequestError(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://127.0.0.1:1/hook?access_token=SECRET", nil)
	require.NoError(t, err)
	_, err = http.DefaultClient.Do(req)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "SECRET"), "net/http quotes the full URL")
	err = RedactRequestError(err)
	assert.NotContains(t, err.Error(), "SECRET")
	assert.Contains(t, err.Error(), "http://127.0.0.1:1")
}
