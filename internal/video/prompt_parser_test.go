package video

import "testing"

func TestIsVideoRequest(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{text: "生成一个海边落日视频", want: true},
		{text: "帮我做个视频，主题是猫咪", want: true},
		{text: "make a video about sunset", want: true},
		{text: "帮我写一首诗", want: false},
		{text: "", want: false},
	}
	for _, tc := range cases {
		if got := IsVideoRequest(tc.text); got != tc.want {
			t.Fatalf("IsVideoRequest(%q)=%v want=%v", tc.text, got, tc.want)
		}
	}
}
