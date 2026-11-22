package web

import "testing"

func TestOpenCodeModelsParsingAndRanking(t *testing.T) {
	rawModels := []byte(`{"data":[
		{"id":"gpt-6-sol","providerID":"openrouter","name":"GPT 6 Sol","variants":[{"id":"low"},{"id":"high"}]},
		{"id":"deepseek-v4-pro","providerID":"opencode-go","name":"DeepSeek V4 Pro","variants":[{"id":"low"},{"id":"medium"},{"id":"max"}]},
		{"id":"claude-opus-5-5","providerID":"devin","name":"Claude Opus 5.5","variants":[{"id":"low"},{"id":"high"}]}
	]}`)
	rawProvs := []byte(`{"data":[{"id":"opencode-go"},{"id":"openrouter"}]}`)

	models := parseOpenCodeModels(rawModels)
	provs := parseOpenCodeProviders(rawProvs)
	cur := ocSessionRef{Agent: "build", ProviderID: "devin", ModelID: "claude-opus-5-5", Variant: "high"}
	opts := ocModelOpts(models, provs, cur)

	if len(opts) != 2 {
		t.Fatalf("expected 2 filtered models (devin + opencode-go, excluding openrouter), got %d: %+v", len(opts), opts)
	}
	if opts[0].Label != "devin/claude-opus-5-5" || !opts[0].Current || len(opts[0].Efforts) != 2 {
		t.Fatalf("opts[0] = %+v", opts[0])
	}
	if opts[1].Label != "opencode-go/deepseek-v4-pro" || opts[1].Current || len(opts[1].Efforts) != 3 {
		t.Fatalf("opts[1] = %+v", opts[1])
	}

	p, id, ok := splitOpenCodeModelRef("opencode-go/deepseek-v4-pro")
	if !ok || p != "opencode-go" || id != "deepseek-v4-pro" {
		t.Fatalf("splitOpenCodeModelRef failed: %q %q %v", p, id, ok)
	}
	if _, _, ok := splitOpenCodeModelRef("bad-no-slash"); ok {
		t.Fatalf("expected false for missing slash")
	}
}
