package llm

import "testing"

func TestThinkStripper(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"no tags", []string{"hello ", "world"}, "hello world"},
		{"single block", []string{"<think>scratch</think>visible"}, "visible"},
		{"block then text", []string{"<think>foo</think>", " hi"}, " hi"},
		{"text then block", []string{"hi ", "<think>foo</think>", " bye"}, "hi  bye"},
		{"split open tag", []string{"hi <thi", "nk>foo</think> done"}, "hi  done"},
		{"split close tag", []string{"<think>foo</thi", "nk> done"}, " done"},
		{"open tag across many chunks", []string{"<", "t", "h", "i", "n", "k", ">", "scratch", "</think>!"}, "!"},
		{"two blocks", []string{"a<think>x</think>b<think>y</think>c"}, "abc"},
		{"unclosed block at EOF (truncation)", []string{"visible <think>still thinking"}, "visible "},
		{"dangling lt at EOF stays visible", []string{"price < 5"}, "price < 5"},
		{"dangling partial open at EOF emits as text", []string{"text <thi"}, "text <thi"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var s thinkStripper
			var got string
			for _, c := range tc.chunks {
				got += s.feed(c)
			}
			got += s.flush()
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
