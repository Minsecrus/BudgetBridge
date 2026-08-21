package proxy

import (
	"encoding/json"
	"testing"
)

func TestParseAffinity_MessagesRawMessage(t *testing.T) {
	cases := []struct {
		name     string
		messages string // the JSON value for "messages" (or empty/nil)
		wantKey  string
		wantWarm bool
	}{
		{
			"first user message text is the key",
			`[{"role":"user","content":"hello affinity"},{"role":"assistant","content":"hi"}]`,
			"hello affinity",
			true, // has an assistant turn
		},
		{
			"cold single-turn: no assistant turn",
			`[{"role":"user","content":"just asking"}]`,
			"just asking",
			false,
		},
		{
			"first user message wins; later user messages ignored",
			`[{"role":"user","content":"first"},{"role":"assistant","content":"a"},{"role":"user","content":"second"}]`,
			"first",
			true,
		},
		{
			"content as array of text blocks",
			`[{"role":"user","content":[{"type":"text","text":"block-"},{"type":"text","text":"one"}]}]`,
			"block-one",
			false,
		},
		{
			"content with tool_result block recurses into its content",
			`[{"role":"user","content":[{"type":"tool_result","content":"recovered"}]}]`,
			"recovered",
			false,
		},
		{
			"empty messages array",
			`[]`,
			"",
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, warm := parseAffinity(json.RawMessage(c.messages))
			if key != c.wantKey {
				t.Errorf("key: got %q, want %q", key, c.wantKey)
			}
			if warm != c.wantWarm {
				t.Errorf("warm: got %v, want %v", warm, c.wantWarm)
			}
		})
	}
}

func TestParseAffinity_NilAndMalformed(t *testing.T) {
	// nil RawMessage (absent "messages" field) → empty/false, matching the old
	// body-unmarshal-error fallback.
	if key, warm := parseAffinity(nil); key != "" || warm {
		t.Errorf("nil messages: got (%q,%v), want (\"\",false)", key, warm)
	}
	// empty RawMessage likewise.
	if key, warm := parseAffinity(json.RawMessage(``)); key != "" || warm {
		t.Errorf("empty messages: got (%q,%v), want (\"\",false)", key, warm)
	}
	// malformed JSON → empty/false, not a panic.
	if key, warm := parseAffinity(json.RawMessage(`{not json`)); key != "" || warm {
		t.Errorf("malformed messages: got (%q,%v), want (\"\",false)", key, warm)
	}
}
