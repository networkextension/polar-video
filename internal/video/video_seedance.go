package video

// Seedance (Volces / Doubao) video-generation provider adapter. Mirrors
// the user's openclaw.sh / poll_results.sh scripts almost line-for-line:
// POST a prompt to /contents/generations/tasks, get a task_id back, then
// GET /contents/generations/tasks/{task_id} on a ticker until the status
// flips to succeeded (and a video_url is returned) or failed/cancelled.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// apiKeyFingerprint returns a non-secret fingerprint of an API key for log
// output: first 4 + last 4 chars when the key is long enough, otherwise
// "(too-short)". Use this so we never accidentally write the full secret
// to disk via log lines.
func apiKeyFingerprint(key string) string {
	trimmed := strings.TrimSpace(key)
	if len(trimmed) < 12 {
		return "(too-short)"
	}
	return trimmed[:4] + "…" + trimmed[len(trimmed)-4:]
}

// SeedanceParams are the per-shot knobs the request body needs. Sourced from
// the per-shot row, falling back to the LLMConfig.Extras blob, falling back
// to the script defaults.
type SeedanceParams struct {
	Ratio         string `json:"ratio"`
	Duration      int    `json:"duration"`
	GenerateAudio bool   `json:"generate_audio"`
	Watermark     bool   `json:"watermark"`
}

// SeedanceDefaults returns the script's defaults so callers always have a
// sane fallback even when the LLMConfig has no extras set.
func SeedanceDefaults() SeedanceParams {
	return SeedanceParams{
		Ratio:         "9:16",
		Duration:      10,
		GenerateAudio: true,
		Watermark:     false,
	}
}

// SeedanceParamsFromExtras parses a config-level extras blob and overlays
// per-shot overrides. Missing fields fall back to defaults. Used both when
// submitting a shot and when seeding the Add-shot form.
func SeedanceParamsFromExtras(extras json.RawMessage, override SeedanceParams) SeedanceParams {
	out := SeedanceDefaults()
	if len(extras) > 0 {
		var parsed SeedanceParams
		if err := json.Unmarshal(extras, &parsed); err == nil {
			if parsed.Ratio != "" {
				out.Ratio = parsed.Ratio
			}
			if parsed.Duration > 0 {
				out.Duration = parsed.Duration
			}
			out.GenerateAudio = parsed.GenerateAudio
			out.Watermark = parsed.Watermark
		}
	}
	if override.Ratio != "" {
		out.Ratio = override.Ratio
	}
	if override.Duration > 0 {
		out.Duration = override.Duration
	}
	// Booleans always overlay so explicit shot-level toggles win.
	out.GenerateAudio = override.GenerateAudio
	out.Watermark = override.Watermark
	return out
}

// seedanceTasksEndpoint returns the canonical /contents/generations/tasks
// path joined onto the configured base URL. Tolerates the user adding or
// omitting a trailing slash.
func seedanceTasksEndpoint(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "https://ark.cn-beijing.volces.com/api/v3"
	}
	if !strings.HasSuffix(base, "/api/v3") && !strings.Contains(base, "/contents/generations") {
		// Be forgiving: if someone pastes the host-only URL we still build
		// a working endpoint instead of failing late at HTTP time.
		base += "/api/v3"
	}
	return base + "/contents/generations/tasks"
}

type seedanceSubmitRequest struct {
	Model          string                  `json:"model"`
	Content        []seedanceContentEntry  `json:"content"`
	Ratio          string                  `json:"ratio"`
	Duration       int                     `json:"duration"`
	GenerateAudio  bool                    `json:"generate_audio"`
	Watermark      bool                    `json:"watermark"`
}

// seedanceContentEntry is a single entry in the multimodal content array.
// type="text" carries Text; type="image_url" carries an ImageURL object
// (Seedance follows the OpenAI-compatible shape: { url: "..." }, not a
// flat string — Volces returns 400 InvalidParameter on the flat form).
type seedanceContentEntry struct {
	Type     string                `json:"type"`
	Text     string                `json:"text,omitempty"`
	ImageURL *seedanceImageURLSpec `json:"image_url,omitempty"`
}

type seedanceImageURLSpec struct {
	URL string `json:"url"`
}

type seedanceSubmitResponse struct {
	ID      string          `json:"id"`
	TaskID  string          `json:"task_id"`
	Error   json.RawMessage `json:"error,omitempty"`
	Message string          `json:"message,omitempty"`
}

type seedancePollResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// Both shapes are observed in the wild, mirror the bash fallback chain.
	Content *seedanceMediaBlock `json:"content,omitempty"`
	Result  *seedanceMediaBlock `json:"result,omitempty"`
	Error   json.RawMessage     `json:"error,omitempty"`
	Message string              `json:"message,omitempty"`
}

type seedanceMediaBlock struct {
	VideoURL string `json:"video_url,omitempty"`
}

// submitSeedanceTask posts one prompt to the Seedance task endpoint and
// returns the new task_id. The model + auth come from the supplied
// LLMConfig. characterRefURL is optional — when set, Seedance receives
// a multimodal content payload (image_url + text) so the generated video
// uses the supplied frame as the character/style reference instead of
// inventing a fresh face every shot.
func submitSeedanceTask(ctx context.Context, client *http.Client, baseURL, apiKey, model, prompt, characterRefURL string, params SeedanceParams) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if strings.TrimSpace(apiKey) == "" {
		return "", errors.New("seedance: missing api key")
	}
	if strings.TrimSpace(model) == "" {
		return "", errors.New("seedance: missing model")
	}
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("seedance: empty prompt")
	}
	finalPrompt := prompt
	content := []seedanceContentEntry{}
	if ref := strings.TrimSpace(characterRefURL); ref != "" {
		// Prepend the reference image first (Seedance expects image_url
		// before text in the content array) and lightly nudge the prompt
		// so the model knows to lock onto the referenced character.
		content = append(content, seedanceContentEntry{
			Type:     "image_url",
			ImageURL: &seedanceImageURLSpec{URL: ref},
		})
		finalPrompt = "基于参考图人物保持外貌一致；" + prompt
	}
	content = append(content, seedanceContentEntry{Type: "text", Text: finalPrompt})
	body, err := json.Marshal(seedanceSubmitRequest{
		Model:         model,
		Content:       content,
		Ratio:         params.Ratio,
		Duration:      params.Duration,
		GenerateAudio: params.GenerateAudio,
		Watermark:     params.Watermark,
	})
	if err != nil {
		return "", err
	}
	endpoint := seedanceTasksEndpoint(baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	// Debug: log a fingerprint of the key actually being sent so the
	// operator can confirm "I updated the key" landed in the DB. Never log
	// the full secret — first/last 4 chars + length is enough to spot the
	// "wrong account" / "missing prefix" mistake.
	log.Printf("seedance submit: endpoint=%s model=%q key_len=%d key_fp=%s prompt_len=%d ratio=%s dur=%d",
		endpoint, model, len(apiKey), apiKeyFingerprint(apiKey), len(prompt), params.Ratio, params.Duration)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("seedance submit http %d: %s", resp.StatusCode, truncateBody(respBody, 400))
	}
	var parsed seedanceSubmitResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("seedance submit decode: %w (body=%s)", err, truncateBody(respBody, 400))
	}
	taskID := strings.TrimSpace(parsed.ID)
	if taskID == "" {
		taskID = strings.TrimSpace(parsed.TaskID)
	}
	if taskID == "" {
		return "", fmt.Errorf("seedance submit: no task id in response (body=%s)", truncateBody(respBody, 400))
	}
	return taskID, nil
}

// pollSeedanceTask asks the Seedance API for the current status of a task
// and returns a normalized status string (one of VideoShotStatus*) plus the
// final video URL when status is succeeded. The bash script's fallback
// chain (.content.video_url -> .result.video_url) is replicated here.
func pollSeedanceTask(ctx context.Context, client *http.Client, baseURL, apiKey, taskID string) (status, videoURL, errorMessage string, err error) {
	if client == nil {
		client = http.DefaultClient
	}
	if strings.TrimSpace(taskID) == "" {
		return "", "", "", errors.New("seedance: empty task id")
	}
	endpoint := seedanceTasksEndpoint(baseURL) + "/" + taskID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", fmt.Errorf("seedance poll http %d: %s", resp.StatusCode, truncateBody(respBody, 400))
	}
	var parsed seedancePollResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", "", "", fmt.Errorf("seedance poll decode: %w (body=%s)", err, truncateBody(respBody, 400))
	}
	normalized := normalizeSeedanceStatus(parsed.Status)
	if normalized == VideoShotStatusSucceeded {
		if parsed.Content != nil && parsed.Content.VideoURL != "" {
			videoURL = parsed.Content.VideoURL
		} else if parsed.Result != nil && parsed.Result.VideoURL != "" {
			videoURL = parsed.Result.VideoURL
		}
		if videoURL == "" {
			// Mirror the script's "succeeded but no URL" branch — surface
			// it as a soft failure so the row gets a useful error message.
			return VideoShotStatusFailed, "", "Seedance reported success but the response carried no video_url", nil
		}
	}
	if normalized == VideoShotStatusFailed {
		errorMessage = truncateBody(respBody, 400)
	}
	return normalized, videoURL, errorMessage, nil
}

// normalizeSeedanceStatus maps the API's status strings into our internal
// shot status taxonomy. Unknown values (the script's "❓ 未知状态" branch)
// fall through as "running" so the worker keeps polling instead of giving
// up prematurely.
func normalizeSeedanceStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "succeeded":
		return VideoShotStatusSucceeded
	case "failed", "cancelled", "canceled":
		return VideoShotStatusFailed
	case "queued":
		return VideoShotStatusQueued
	case "running", "in_progress", "pending", "":
		return VideoShotStatusRunning
	default:
		return VideoShotStatusRunning
	}
}

// downloadVideoToTemp streams a finished video URL to a temp file and
// returns the path. The poll worker hands it off to the Storage interface
// (local or R2) immediately afterwards.
func downloadVideoToTemp(ctx context.Context, client *http.Client, videoURL, suggestedExt string) (string, int64, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, videoURL, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("seedance download http %d", resp.StatusCode)
	}
	if suggestedExt == "" {
		suggestedExt = ".mp4"
	}
	tmp, err := osCreateTempVideoFile(suggestedExt)
	if err != nil {
		return "", 0, err
	}
	written, err := io.Copy(tmp, resp.Body)
	if cerr := tmp.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	return tmp.Name(), written, nil
}

func truncateBody(body []byte, n int) string {
	s := string(body)
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// guard against accidental import-cycle: this file mustn't import the
// outer app package. The unused-context import is here as a safety net so
// linters keep complaining if someone removes the ctx-aware HTTP path.
var _ = context.Background
var _ = time.Second
