package video

import "strings"

var videoKeywords = []string{
	"生成视频",
	"做个视频",
	"图生视频",
	"短视频",
	"文生视频",
	"video",
}

func IsVideoRequest(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return false
	}
	for _, keyword := range videoKeywords {
		if strings.Contains(normalized, keyword) {
			return true
		}
	}
	if strings.Contains(normalized, "视频") && strings.Contains(normalized, "生成") {
		return true
	}
	return false
}

func NormalizePrompt(text string) string {
	return strings.TrimSpace(text)
}
