package proxy

import "testing"

func TestExtractUsage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want *tokenUsage
	}{
		{
			"no usage block",
			`{"choices":[{"delta":{"content":"hi"}}]}`,
			nil,
		},
		{
			"plain usage",
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`,
			&tokenUsage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
		},
		{
			"top-level cached_tokens",
			`{"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"cached_tokens":80}}`,
			&tokenUsage{PromptTokens: 100, CompletionTokens: 5, TotalTokens: 105, CachedTokens: 80},
		},
		{
			"nested prompt_tokens_details.cached_tokens",
			`{"usage":{"prompt_tokens":100,"total_tokens":100,"prompt_tokens_details":{"cached_tokens":42}}}`,
			&tokenUsage{PromptTokens: 100, TotalTokens: 100, CachedTokens: 42},
		},
		{
			"anthropic-style cache_read_input_tokens",
			`{"usage":{"prompt_tokens":100,"cache_read_input_tokens":17}}`,
			&tokenUsage{PromptTokens: 100, CachedTokens: 17},
		},
		{
			"all-zero usage → nil",
			`{"usage":{"prompt_tokens":0}}`,
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractUsage([]byte(c.body))
			if got == nil {
				if c.want != nil {
					t.Fatalf("got nil, want %+v", c.want)
				}
				return
			}
			if c.want == nil {
				t.Fatalf("got %+v, want nil", got)
			}
			if *got != *c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestHashKey(t *testing.T) {
	if hashKey("") != "" {
		t.Fatal("empty key should hash to empty")
	}
	a := hashKey("hello world")
	b := hashKey("hello world")
	c := hashKey("hello earth")
	if len(a) != 8 || a != b {
		t.Fatalf("hash not stable/8-char: %q vs %q", a, b)
	}
	if a == c {
		t.Fatal("distinct inputs collided")
	}
}

func TestTokenUsageFromFields(t *testing.T) {
	cases := []struct {
		name                                                 string
		prompt, completion, total, cached, nested, cacheRead int
		want                                                 *tokenUsage
	}{
		{"all zero → nil", 0, 0, 0, 0, 0, 0, nil},
		{"plain", 10, 20, 30, 0, 0, 0, &tokenUsage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}},
		{"top-level cached wins", 100, 5, 105, 80, 7, 9, &tokenUsage{PromptTokens: 100, CompletionTokens: 5, TotalTokens: 105, CachedTokens: 80}},
		{"nested cached fallback", 100, 0, 100, 0, 42, 9, &tokenUsage{PromptTokens: 100, TotalTokens: 100, CachedTokens: 42}},
		{"cache_read fallback", 100, 0, 0, 0, 0, 17, &tokenUsage{PromptTokens: 100, CachedTokens: 17}},
		{"completion-only non-zero", 0, 5, 0, 0, 0, 0, &tokenUsage{CompletionTokens: 5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tokenUsageFromFields(c.prompt, c.completion, c.total, c.cached, c.nested, c.cacheRead)
			if got == nil {
				if c.want != nil {
					t.Fatalf("got nil, want %+v", c.want)
				}
				return
			}
			if c.want == nil {
				t.Fatalf("got %+v, want nil", got)
			}
			if *got != *c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}
