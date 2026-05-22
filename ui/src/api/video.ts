// Typed wrappers for the /api/video-* endpoints. All requests go through
// the shared http.ts helper so the access-token refresh interceptor and
// credential-include settings apply uniformly.

import { request, requestJson } from "./http.js";
import type {
  VideoLLMConfigListResponse,
  VideoProjectDetailResponse,
  VideoProjectListResponse,
  VideoSingleAssetResponse,
  VideoSingleProjectResponse,
  VideoSingleShotResponse,
  VideoSubmitAllResponse,
} from "../types/video.js";

export async function fetchVideoLLMConfigs() {
  return requestJson<VideoLLMConfigListResponse>("/api/video-llm-configs");
}

export async function fetchVideoProjects() {
  return requestJson<VideoProjectListResponse>("/api/video-projects");
}

export async function fetchVideoProject(id: number) {
  return requestJson<VideoProjectDetailResponse>(`/api/video-projects/${id}`);
}

export async function createVideoProject(title: string, defaultLLMConfigID?: number | null) {
  return requestJson<VideoSingleProjectResponse>("/api/video-projects", {
    method: "POST",
    body: {
      title,
      default_llm_config_id: defaultLLMConfigID && defaultLLMConfigID > 0 ? defaultLLMConfigID : undefined,
    },
  });
}

export async function updateVideoProject(id: number, body: { title?: string; default_llm_config_id?: number | null }) {
  return requestJson<VideoSingleProjectResponse>(`/api/video-projects/${id}`, {
    method: "PATCH",
    body,
  });
}

export async function deleteVideoProject(id: number) {
  return request(`/api/video-projects/${id}`, { method: "DELETE" });
}

export type CreateVideoShotInput = {
  prompt: string;
  ratio?: string;
  duration?: number;
  generate_audio?: boolean;
  watermark?: boolean;
  llm_config_id?: number | null;
};

export async function createVideoShot(projectID: number, body: CreateVideoShotInput) {
  return requestJson<VideoSingleShotResponse>(`/api/video-projects/${projectID}/shots`, {
    method: "POST",
    body,
  });
}

export type UpdateVideoShotInput = {
  prompt?: string;
  ratio?: string;
  duration?: number;
  generate_audio?: boolean;
  watermark?: boolean;
  ord?: number;
  llm_config_id?: number | null;
  trim_start_ms?: number;
  trim_end_ms?: number;
};

export async function updateVideoShot(projectID: number, shotID: number, body: UpdateVideoShotInput) {
  return requestJson<VideoSingleShotResponse>(`/api/video-projects/${projectID}/shots/${shotID}`, {
    method: "PATCH",
    body,
  });
}

export async function deleteVideoShot(projectID: number, shotID: number) {
  return request(`/api/video-projects/${projectID}/shots/${shotID}`, { method: "DELETE" });
}

export async function submitVideoShot(projectID: number, shotID: number) {
  return requestJson<VideoSingleShotResponse>(`/api/video-projects/${projectID}/shots/${shotID}/submit`, {
    method: "POST",
  });
}

export async function retryVideoShot(projectID: number, shotID: number) {
  return requestJson<VideoSingleShotResponse>(`/api/video-projects/${projectID}/shots/${shotID}/retry`, {
    method: "POST",
  });
}

export async function duplicateVideoShot(projectID: number, shotID: number) {
  return requestJson<VideoSingleShotResponse>(`/api/video-projects/${projectID}/shots/${shotID}/duplicate`, {
    method: "POST",
  });
}

export async function extractCharacterFrame(projectID: number, shotID: number, timestampMs: number) {
  return requestJson<VideoSingleAssetResponse>(`/api/video-projects/${projectID}/shots/${shotID}/extract-frame`, {
    method: "POST",
    body: { timestamp_ms: Math.max(0, Math.round(timestampMs)) },
  });
}

export async function submitAllVideoShots(projectID: number) {
  return requestJson<VideoSubmitAllResponse>(`/api/video-projects/${projectID}/submit-all`, {
    method: "POST",
  });
}

export async function uploadVideoAsset(projectID: number, kind: "audio_bgm" | "voiceover", file: File) {
  const formData = new FormData();
  formData.append("file", file);
  return requestJson<VideoSingleAssetResponse>(`/api/video-projects/${projectID}/assets?kind=${encodeURIComponent(kind)}`, {
    method: "POST",
    body: formData,
  });
}

export async function updateVideoAssetVolumes(projectID: number, assetID: number, body: { bgm_volume?: number; voice_volume?: number }) {
  return requestJson<VideoSingleAssetResponse>(`/api/video-projects/${projectID}/assets/${assetID}`, {
    method: "PATCH",
    body,
  });
}

export async function deleteVideoAsset(projectID: number, assetID: number) {
  return request(`/api/video-projects/${projectID}/assets/${assetID}`, { method: "DELETE" });
}

export async function renderVideoProject(projectID: number) {
  return request(`/api/video-projects/${projectID}/render`, { method: "POST" });
}
