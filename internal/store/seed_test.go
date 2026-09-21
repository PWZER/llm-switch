package store

import (
	"context"
	"testing"
)

func TestSeedDefaultProviders(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	seeded, err := st.SeedDefaultProviders(ctx)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !seeded {
		t.Fatal("first seed should report seeded=true")
	}

	providers, err := st.Providers.List(ctx)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(providers) != len(defaultProviders) {
		t.Fatalf("want %d providers, got %d", len(defaultProviders), len(providers))
	}

	channels, err := st.Channels.List(ctx)
	if err != nil {
		t.Fatalf("list channels: %v", err)
	}
	wantChannels := 0
	for _, p := range defaultProviders {
		wantChannels += len(p.Channels)
	}
	if len(channels) != wantChannels {
		t.Fatalf("want %d channels, got %d", wantChannels, len(channels))
	}

	// Channels are keyed by (provider, protocol) — the map must absorb every
	// row, i.e. no provider carries a second channel of one protocol.
	byProto := map[string]Channel{}
	for _, c := range channels {
		byProto[c.ProviderName+"/"+c.Protocol] = c
	}
	if len(byProto) != len(channels) {
		t.Fatalf("seeded duplicate (provider, protocol) channel: %d rows, %d keys", len(channels), len(byProto))
	}

	// Spot-check the tricky endpoints: Kimi For Coding's significant trailing
	// slash, OpenAI's two protocol-keyed channels, and protocol-default auth
	// styles (seeded channels all carry '' — dispatch resolves bearer/x-api-key
	// per protocol).
	za := byProto["Zhipu GLM/anthropic"]
	if za.BaseURL != "https://open.bigmodel.cn/api/anthropic" || za.ChatPath != "/v1/messages" || za.AuthStyle != "" {
		t.Fatalf("zhipu anthropic wrong: %+v", za)
	}
	kc := byProto["KimiCoding/anthropic"]
	if kc.BaseURL != "https://api.kimi.com/coding/" {
		t.Fatalf("kimi coding base_url lost trailing slash: %q", kc.BaseURL)
	}
	oa := byProto["OpenAI/openai"]
	if oa.BaseURL != "https://api.openai.com/v1" || oa.ChatPath != "/chat/completions" {
		t.Fatalf("openai chat channel wrong: %+v", oa)
	}
	or := byProto["OpenAI/responses"]
	if or.BaseURL != "https://api.openai.com/v1" || or.ChatPath != "/responses" {
		t.Fatalf("openai responses channel wrong: %+v", or)
	}

	// Every seeded provider carries its docs-verified models-list fetch URL.
	for _, p := range providers {
		if p.ModelsURL == nil || *p.ModelsURL == "" {
			t.Fatalf("seeded provider %q missing models_url", p.Name)
		}
	}

	// Seed channels start with an empty model registry by design — rows only
	// appear via preview-register or the models page.
	models, err := st.Models.List(ctx)
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("seed must not create model rows, got %+v", models)
	}

	// Second call is a no-op guarded by the seeded_providers setting.
	if _, err := st.SeedDefaultProviders(ctx); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	providers, err = st.Providers.List(ctx)
	if err != nil || len(providers) != len(defaultProviders) {
		t.Fatalf("re-seed duplicated providers: %d (err=%v)", len(providers), err)
	}

	// Deleting every provider must NOT resurrect the defaults on next boot.
	for _, p := range providers {
		if err := st.DeleteProvider(ctx, p.ID); err != nil {
			t.Fatalf("delete provider: %v", err)
		}
	}
	if _, err := st.SeedDefaultProviders(ctx); err != nil {
		t.Fatalf("seed after wipe: %v", err)
	}
	providers, err = st.Providers.List(ctx)
	if err != nil || len(providers) != 0 {
		t.Fatalf("seed resurrected deleted providers: %d (err=%v)", len(providers), err)
	}
}
