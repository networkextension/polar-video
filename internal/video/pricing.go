package video

// pricing.go — per-shot pricing for video generation. Computed locally
// (no provider usage API today) using a static table per provider.
//
// Why a local table:
//
//   - Seedance API doesn't return token / cost info on the task
//     poll envelope; we have to compute from the request parameters.
//   - Provider price changes are rare and operator-controlled — we'd
//     rather pin the rate in a code-reviewed file than fetch from the
//     vendor.
//
// The result blob (`billing_meta`) captures the exact pricing-table
// snapshot used so a future audit can reproduce the math.

import (
	"encoding/json"
	"strings"
	"time"
)

// ShotPricingInput is everything the priceShot function needs to
// produce a cost number. The Audio + Resolution + DurationSec come
// from the shot row; Provider + Model come from the LLMConfig that
// generated it.
type ShotPricingInput struct {
	Provider    string
	Model       string
	Resolution  string
	DurationSec int
	Audio       bool
}

// ShotPricingResult is the per-shot output the poll worker hands to
// markVideoShotBilled. FPS + FramesTotal are derived from provider
// defaults — Seedance doesn't return either, so we hardcode 24 fps.
// CostPerFrameUSD is pre-computed so analytics queries stay simple.
type ShotPricingResult struct {
	CostUSD         float64
	CostPerFrameUSD float64
	FPS             int
	FramesTotal     int
	// BillingMeta is serialised to the billing_meta JSONB column.
	// Always include `priced_at` + `provider` + `reason` for misses,
	// plus the exact rate the cost was derived from when matched.
	BillingMeta map[string]any
}

// seedanceRate is keyed by (model, resolution, audio). Resolution is
// taken from the shot row (caller falls back to "720p" if unknown).
// Rates in USD per billed second; provider rounds up to whole seconds.
//
// Source: Volces Doubao pricing page snapshot, 2026-05. Update this
// table when the vendor changes prices; the billing_meta JSONB on
// each row captures the rate used so prior history is unaffected.
var seedanceRate = map[seedanceKey]float64{
	{model: "doubao-seedance-1-0-pro-250528", resolution: "480p", audio: false}: 0.0089,
	{model: "doubao-seedance-1-0-pro-250528", resolution: "480p", audio: true}:  0.0089,
	{model: "doubao-seedance-1-0-pro-250528", resolution: "720p", audio: false}: 0.020,
	{model: "doubao-seedance-1-0-pro-250528", resolution: "720p", audio: true}:  0.025,
	{model: "doubao-seedance-1-0-pro-250528", resolution: "1080p", audio: false}: 0.040,
	{model: "doubao-seedance-1-0-pro-250528", resolution: "1080p", audio: true}:  0.050,
	// Generic fallback rates keyed on resolution only — used when the
	// exact model id isn't in the table (e.g. a newer revision). Keeps
	// us from silently writing $0 for shots that did real work.
	{model: "*seedance*", resolution: "720p", audio: false}: 0.020,
	{model: "*seedance*", resolution: "720p", audio: true}:  0.025,
	{model: "*seedance*", resolution: "1080p", audio: false}: 0.040,
	{model: "*seedance*", resolution: "1080p", audio: true}:  0.050,
}

type seedanceKey struct {
	model      string
	resolution string
	audio      bool
}

const seedanceDefaultFPS = 24

// PriceShot computes the per-shot cost using the provider-specific
// pricing table. Returns CostUSD=0 + reason="no_price" in BillingMeta
// when the provider/model isn't recognised so the row still records
// frames + duration for later forensic review.
func PriceShot(in ShotPricingInput) ShotPricingResult {
	switch in.Provider {
	case LLMConfigKindVideoSeedance:
		return priceSeedance(in)
	default:
		return ShotPricingResult{
			FPS:         seedanceDefaultFPS,
			BillingMeta: map[string]any{
				"reason":    "unknown_provider",
				"provider":  in.Provider,
				"priced_at": time.Now().UTC().Format(time.RFC3339),
			},
		}
	}
}

func priceSeedance(in ShotPricingInput) ShotPricingResult {
	billedSec := in.DurationSec
	if billedSec < 0 {
		billedSec = 0
	}
	resolution := in.Resolution
	if resolution == "" {
		resolution = "720p"
	}
	fps := seedanceDefaultFPS
	framesTotal := billedSec * fps

	key := seedanceKey{model: in.Model, resolution: resolution, audio: in.Audio}
	rate, ok := seedanceRate[key]
	matchedModel := in.Model
	if !ok {
		// Try the wildcard "*seedance*" model entry as a fallback so
		// we don't drop to $0 on a model revision bump.
		wild := seedanceKey{model: "*seedance*", resolution: resolution, audio: in.Audio}
		if r, wok := seedanceRate[wild]; wok && strings.Contains(strings.ToLower(in.Model), "seedance") {
			rate = r
			ok = true
			matchedModel = "*seedance*"
		}
	}

	meta := map[string]any{
		"provider":      LLMConfigKindVideoSeedance,
		"model":         in.Model,
		"matched_model": matchedModel,
		"resolution":    resolution,
		"audio":         in.Audio,
		"duration_sec":  billedSec,
		"fps":           fps,
		"priced_at":     time.Now().UTC().Format(time.RFC3339),
	}
	if !ok {
		meta["reason"] = "no_price"
		return ShotPricingResult{
			FPS:         fps,
			FramesTotal: framesTotal,
			BillingMeta: meta,
		}
	}
	cost := rate * float64(billedSec)
	meta["rate_usd_per_sec"] = rate
	var perFrame float64
	if framesTotal > 0 {
		perFrame = cost / float64(framesTotal)
	}
	return ShotPricingResult{
		CostUSD:         cost,
		CostPerFrameUSD: perFrame,
		FPS:             fps,
		FramesTotal:     framesTotal,
		BillingMeta:     meta,
	}
}

// ResolutionFromShot infers the resolution string the pricing table
// expects from a shot's params. Seedance's `ratio` field doesn't
// pin resolution (9:16 can be 720x1280 or 1080x1920 etc.); operators
// override per-LLMConfig via the Extras blob's "resolution" key.
// Falls back to "720p" — Seedance's documented default.
func ResolutionFromShot(extras []byte) string {
	if len(extras) == 0 {
		return "720p"
	}
	var parsed struct {
		Resolution string `json:"resolution"`
	}
	if err := json.Unmarshal(extras, &parsed); err != nil {
		return "720p"
	}
	if s := strings.TrimSpace(parsed.Resolution); s != "" {
		return s
	}
	return "720p"
}

// MarshalBillingMeta is the conversion helper used by the store
// when writing billing_meta to the JSONB column. Returns nil on a
// nil/empty map so the column gets a SQL NULL rather than `{}`.
func MarshalBillingMeta(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	return json.Marshal(m)
}
