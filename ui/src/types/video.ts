// Type contracts for the video-studio module. Mirrors the Go structs in
// internal/app/dock/store.go and the JSON shapes returned by handlers in
// internal/app/dock/video_studio.go.

export type VideoLLMConfig = {
  id: number;
  owner_user_id: string;
  share_id: string;
  shared: boolean;
  name: string;
  base_url: string;
  model: string;
  has_api_key: boolean;
  provider_kind: string;
  extras?: unknown;
  created_at: string;
  updated_at: string;
};

export type VideoLLMConfigListResponse = {
  configs?: VideoLLMConfig[];
  error?: string;
};

export type VideoProject = {
  id: number;
  owner_user_id: string;
  title: string;
  default_llm_config_id?: number;
  status: "draft" | "rendering" | "rendered" | "failed";
  final_video_url?: string;
  final_render_error?: string;
  created_at: string;
  updated_at: string;
};

export type VideoShot = {
  id: number;
  project_id: number;
  ord: number;
  prompt: string;
  ratio: string;
  duration: number;
  generate_audio: boolean;
  watermark: boolean;
  llm_config_id?: number;
  task_id?: string;
  status: "pending" | "queued" | "running" | "succeeded" | "failed";
  video_url?: string;
  poster_url?: string;
  trim_start_ms: number;
  trim_end_ms: number;
  error_message?: string;
  submitted_at?: string;
  completed_at?: string;
  created_at: string;
  updated_at: string;
};

export type VideoAsset = {
  id: number;
  project_id: number;
  kind: "audio_bgm" | "voiceover" | "character_reference";
  url: string;
  file_name: string;
  mime_type: string;
  size: number;
  duration_ms: number;
  bgm_volume: number;
  voice_volume: number;
  created_at: string;
};

export type VideoProjectListResponse = {
  projects?: VideoProject[];
  error?: string;
};

export type VideoProjectDetailResponse = {
  project?: VideoProject;
  shots?: VideoShot[];
  assets?: VideoAsset[];
  error?: string;
};

export type VideoSingleProjectResponse = {
  project?: VideoProject;
  error?: string;
};

export type VideoSingleShotResponse = {
  shot?: VideoShot;
  error?: string;
};

export type VideoSingleAssetResponse = {
  asset?: VideoAsset;
  error?: string;
};

export type VideoSubmitAllResponse = {
  // Async fan-out: handler returns immediately and the goroutine submits
  // each shot in the background, broadcasting status via WS as it goes.
  queued?: number;
  shot_ids?: number[];
  async?: boolean;
  error?: string;
};

// WebSocket payload (multiplexed on the existing chat WS).
export type VideoStudioEventPayload = {
  type?: string;
  project_id?: number;
  kind?: "shot_status" | "render_status";
  payload?: {
    shot_id?: number;
    status?: string;
    video_url?: string;
    error_message?: string;
    final_video_url?: string;
    final_render_error?: string;
  };
};
