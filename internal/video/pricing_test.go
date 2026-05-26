package video

import (
	"encoding/json"
	"testing"
)

func TestPriceShot_SeedanceKnownSKU(t *testing.T) {
	got := PriceShot(ShotPricingInput{
		Provider:    LLMConfigKindVideoSeedance,
		Model:       "doubao-seedance-1-0-pro-250528",
		Resolution:  "1080p",
		DurationSec: 10,
		Audio:       true,
	})
	if got.CostUSD != 0.050*10 {
		t.Fatalf("cost: want 0.50, got %v", got.CostUSD)
	}
	if got.FPS != 24 {
		t.Fatalf("fps: want 24, got %d", got.FPS)
	}
	if got.FramesTotal != 240 {
		t.Fatalf("frames: want 240, got %d", got.FramesTotal)
	}
	wantPerFrame := got.CostUSD / float64(got.FramesTotal)
	if got.CostPerFrameUSD != wantPerFrame {
		t.Fatalf("per-frame: want %v, got %v", wantPerFrame, got.CostPerFrameUSD)
	}
	if got.BillingMeta["rate_usd_per_sec"] != 0.050 {
		t.Fatalf("rate not surfaced in meta: %v", got.BillingMeta)
	}
}

func TestPriceShot_SeedanceAudioToggleChangesRate(t *testing.T) {
	noAudio := PriceShot(ShotPricingInput{
		Provider: LLMConfigKindVideoSeedance, Model: "doubao-seedance-1-0-pro-250528",
		Resolution: "720p", DurationSec: 5, Audio: false,
	})
	withAudio := PriceShot(ShotPricingInput{
		Provider: LLMConfigKindVideoSeedance, Model: "doubao-seedance-1-0-pro-250528",
		Resolution: "720p", DurationSec: 5, Audio: true,
	})
	if withAudio.CostUSD <= noAudio.CostUSD {
		t.Fatalf("audio variant should cost more: noAudio=%v, withAudio=%v", noAudio.CostUSD, withAudio.CostUSD)
	}
}

func TestPriceShot_SeedanceWildcardFallback(t *testing.T) {
	got := PriceShot(ShotPricingInput{
		Provider: LLMConfigKindVideoSeedance,
		Model:    "doubao-seedance-1-0-pro-FUTURE-REV",
		Resolution: "720p", DurationSec: 5, Audio: false,
	})
	if got.CostUSD == 0 {
		t.Fatalf("wildcard fallback should price unknown seedance rev: got %v", got.CostUSD)
	}
	if got.BillingMeta["matched_model"] != "*seedance*" {
		t.Fatalf("expected wildcard match in meta, got %v", got.BillingMeta)
	}
}

func TestPriceShot_NoPriceStillReturnsFrames(t *testing.T) {
	got := PriceShot(ShotPricingInput{
		Provider: LLMConfigKindVideoSeedance, Model: "totally-not-a-real-model",
		Resolution: "9000p", DurationSec: 10, Audio: false,
	})
	if got.CostUSD != 0 {
		t.Fatalf("unknown SKU should bill 0, got %v", got.CostUSD)
	}
	if got.FramesTotal == 0 {
		t.Fatalf("frames should still be computed for forensic review")
	}
	if got.BillingMeta["reason"] != "no_price" {
		t.Fatalf("expected reason=no_price, got %v", got.BillingMeta)
	}
}

func TestPriceShot_UnknownProvider(t *testing.T) {
	got := PriceShot(ShotPricingInput{Provider: "video.runway", Model: "gen-3"})
	if got.CostUSD != 0 {
		t.Fatalf("unknown provider should bill 0, got %v", got.CostUSD)
	}
	if got.BillingMeta["reason"] != "unknown_provider" {
		t.Fatalf("expected reason=unknown_provider, got %v", got.BillingMeta)
	}
}

func TestResolutionFromShot_DefaultsTo720p(t *testing.T) {
	if got := ResolutionFromShot(nil); got != "720p" {
		t.Fatalf("nil extras: want 720p, got %q", got)
	}
	if got := ResolutionFromShot([]byte("{}")); got != "720p" {
		t.Fatalf("empty extras: want 720p, got %q", got)
	}
}

func TestResolutionFromShot_HonoursOverride(t *testing.T) {
	extras, _ := json.Marshal(map[string]any{"resolution": "1080p"})
	if got := ResolutionFromShot(extras); got != "1080p" {
		t.Fatalf("override: want 1080p, got %q", got)
	}
}

func TestMarshalBillingMeta_NilForEmpty(t *testing.T) {
	if got, err := MarshalBillingMeta(nil); err != nil || got != nil {
		t.Fatalf("nil map: want nil/nil, got (%v, %v)", got, err)
	}
}

func TestMarshalBillingMeta_RoundTrip(t *testing.T) {
	in := map[string]any{"provider": "video.seedance", "rate": 0.05}
	raw, err := MarshalBillingMeta(in)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back["provider"] != "video.seedance" {
		t.Fatalf("round-trip lost provider: %v", back)
	}
}
