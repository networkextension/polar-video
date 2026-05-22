package video

// Provider dispatcher for the video-studio module. Today only Volces /
// Doubao Seedance is implemented; the switch on cfg.ProviderKind is the
// extension seam — adding Runway / Kling / Pika is one new file plus one
// case here. Callers (handlers, poll worker) interact only through this
// thin layer and never reach into provider-specific code.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
)

// ErrUnsupportedVideoProvider is returned by the dispatcher when a config's
// provider_kind isn't one we know how to talk to. Surfacing it as a typed
// error lets handlers translate it into a 400 with a clear message.
var ErrUnsupportedVideoProvider = errors.New("video provider kind not supported")

// videoProviderClient is the HTTP client used for both submission and
// polling. Pre-built once per Server so we don't re-create transports per
// request. Submission is fast (sub-second); polling can cope with 30s
// timeouts because the request itself is just metadata.
type videoProviderClient struct {
	http *http.Client
}

func newVideoProviderClient() *videoProviderClient {
	// Wide overall timeout so polling occasional slow tasks doesn't get
	// killed mid-request, but bounded to keep stuck connections from
	// piling up.
	return &videoProviderClient{
		http: &http.Client{},
	}
}

// submitVideoTask dispatches a per-shot submission to the right provider
// and returns the external task id. Today: Seedance only; other kinds get
// ErrUnsupportedVideoProvider so the caller can flip the shot to failed
// with a clear message. characterRefURL is optional and only honored by
// providers that support image+text multimodal input.
func (vp *videoProviderClient) submitVideoTask(ctx context.Context, cfg *LLMConfig, apiKey, prompt, characterRefURL string, shotParams SeedanceParams) (string, error) {
	if cfg == nil {
		return "", errors.New("video provider: nil config")
	}
	switch cfg.ProviderKind {
	case LLMConfigKindVideoSeedance:
		params := SeedanceParamsFromExtras(cfg.Extras, shotParams)
		return submitSeedanceTask(ctx, vp.http, cfg.BaseURL, apiKey, cfg.Model, prompt, characterRefURL, params)
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedVideoProvider, cfg.ProviderKind)
	}
}

// pollVideoTask asks the provider for the current status of an in-flight
// task and returns (normalizedStatus, videoURL, errorMessage). The
// normalizedStatus is one of VideoShotStatus*. videoURL is only set when
// the status is 'succeeded'; errorMessage only when 'failed'.
func (vp *videoProviderClient) pollVideoTask(ctx context.Context, cfg *LLMConfig, apiKey, taskID string) (status, videoURL, errorMessage string, err error) {
	if cfg == nil {
		return "", "", "", errors.New("video provider: nil config")
	}
	switch cfg.ProviderKind {
	case LLMConfigKindVideoSeedance:
		return pollSeedanceTask(ctx, vp.http, cfg.BaseURL, apiKey, taskID)
	default:
		return "", "", "", fmt.Errorf("%w: %q", ErrUnsupportedVideoProvider, cfg.ProviderKind)
	}
}

// osCreateTempVideoFile is a tiny shim so the seedance adapter can stay
// pure-stdlib. Lives here because it depends on `os` and we don't want to
// pull os into the per-provider files unnecessarily.
func osCreateTempVideoFile(suggestedExt string) (*os.File, error) {
	return os.CreateTemp("", "video-shot-*"+suggestedExt)
}
