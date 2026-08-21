package proxy

import (
	"encoding/json"
	"testing"
)

// TestCacheControl_PreservesPrefixCache verifies the fix: when an Anthropic
// client sends cache_control markers (the SDK default for prompt caching), the
// translator must emit plain-string content, NOT array-form content.
//
// Why: dashscope's prefix cache keys on the tokenized prompt prefix. Array-form
// content tokenizes differently from the plain string OpenAI clients send, so
// emitting array form for Anthropic clients silently dropped cache_read to 0
// when switching OpenAI→Anthropic. The plain-string form is what OpenAI clients
// have always sent, so it keeps the cache key identical across both client
// shapes and across accounts (the scheduler pins a conversation to one account
// precisely to reuse that prefix cache).
//
// cache_control markers are intentionally discarded — dashscope applies prefix
// caching to the leading text regardless of form, and the marker has no
// upstream meaning in the OpenAI request we build.
func TestCacheControl_PreservesPrefixCache(t *testing.T) {
	// User message with a cache_control marker — what the Anthropic SDK sends.
	content := `[{"type":"text","text":"hello world","cache_control":{"type":"ephemeral"}}]`

	var raw json.RawMessage = []byte(content)
	msgs := anthropicMsgToOAI("user", raw)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}

	// Fixed behavior: content is a plain string, not an array.
	var s string
	if err := json.Unmarshal(msgs[0].Content, &s); err != nil {
		t.Fatalf("expected plain-string content (the fix); got non-string: %s", msgs[0].Content)
	}
	if s != "hello world" {
		t.Fatalf("text mismatch: got %q want %q", s, "hello world")
	}
	// No cache_control marker leaked into the OpenAI content.
	if jsonContains(msgs[0].Content, "cache_control") {
		t.Fatalf("cache_control marker leaked into content: %s", msgs[0].Content)
	}
}

// TestAnthropicSystemContent_PlainString is the system-prompt half of the same
// fix. A system array with cache_control must collapse to a plain string.
func TestAnthropicSystemContent_PlainString(t *testing.T) {
	sys := `[{"type":"text","text":"you are helpful","cache_control":{"type":"ephemeral"}}]`
	out := anthropicSystemContent(json.RawMessage(sys))

	var s string
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("expected plain-string system (the fix); got non-string: %s", out)
	}
	if s != "you are helpful" {
		t.Fatalf("system text mismatch: got %q want %q", s, "you are helpful")
	}
	if jsonContains(out, "cache_control") {
		t.Fatalf("cache_control leaked into system: %s", out)
	}
}

// TestAnthropicSystemContent_PlainStringPassthrough: a plain-string system
// passes through unchanged (still a plain string).
func TestAnthropicSystemContent_PlainStringPassthrough(t *testing.T) {
	out := anthropicSystemContent(json.RawMessage(`"hi there"`))
	var s string
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("plain-string passthrough broke: %s", out)
	}
	if s != "hi there" {
		t.Fatalf("got %q want %q", s, "hi there")
	}
}

// jsonContains reports whether the JSON value contains a given substring. Used
// only to assert a marker is ABSENT — checking the raw bytes is fine because
// cache_control would only appear as a key name.
func jsonContains(raw json.RawMessage, substr string) bool {
	return bytesContains(raw, substr)
}

func bytesContains(b []byte, substr string) bool {
	for i := 0; i+len(substr) <= len(b); i++ {
		if string(b[i:i+len(substr)]) == substr {
			return true
		}
	}
	return false
}
