// Video Studio page entrypoint. Three responsibilities:
//   1. CRUD UI for projects / shots / assets via /api/video-* endpoints.
//   2. MediaRecorder voiceover booth so the user can narrate without
//      leaving the page.
//   3. WebSocket subscription on the existing /ws/chat connection — chat
//      events ignore unknown `type` values, so we multiplex video-studio
//      events on the same socket without doubling the server-side hub.

import {
  createVideoProject,
  createVideoShot,
  deleteVideoAsset,
  deleteVideoProject,
  deleteVideoShot,
  duplicateVideoShot,
  extractCharacterFrame,
  fetchVideoLLMConfigs,
  fetchVideoProject,
  fetchVideoProjects,
  renderVideoProject,
  retryVideoShot,
  submitAllVideoShots,
  submitVideoShot,
  updateVideoAssetVolumes,
  updateVideoProject,
  updateVideoShot,
  uploadVideoAsset,
} from "./api/video.js";
import { fetchCurrentUser, logout } from "./api/session.js";
import { byId } from "./lib/dom.js";
import { hydrateSiteBrand, renderSidebarFoot } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";
import { t } from "./lib/i18n.js";
import type {
  VideoAsset,
  VideoLLMConfig,
  VideoProject,
  VideoShot,
  VideoStudioEventPayload,
} from "./types/video.js";

initStoredTheme();
bindThemeSync();

const projectListEl = byId<HTMLElement>("videoProjectList");
const newProjectBtn = byId<HTMLButtonElement>("videoNewProjectBtn");
const studioEmpty = byId<HTMLElement>("videoStudioEmpty");
const studioProject = byId<HTMLElement>("videoStudioProject");
const projectTitleInput = byId<HTMLInputElement>("videoProjectTitle");
const projectStatusEl = byId<HTMLElement>("videoProjectStatus");
const projectConfigSelect = byId<HTMLSelectElement>("videoProjectConfigSelect");
const submitAllBtn = byId<HTMLButtonElement>("videoSubmitAllBtn");
const downloadAllBtn = byId<HTMLButtonElement>("videoDownloadAllBtn");
const renderBtn = byId<HTMLButtonElement>("videoRenderBtn");
const deleteProjectBtn = byId<HTMLButtonElement>("videoDeleteProjectBtn");
const addShotBtn = byId<HTMLButtonElement>("videoAddShotBtn");
const bulkImportBtn = byId<HTMLButtonElement>("videoBulkImportBtn");
const bulkImportPanel = byId<HTMLElement>("videoBulkImportPanel");
const bulkImportText = byId<HTMLTextAreaElement>("videoBulkImportText");
const bulkImportPreview = byId<HTMLElement>("videoBulkImportPreview");
const bulkImportCancelBtn = byId<HTMLButtonElement>("videoBulkImportCancelBtn");
const bulkImportConfirmBtn = byId<HTMLButtonElement>("videoBulkImportConfirmBtn");
const shotListEl = byId<HTMLElement>("videoShotList");
const referenceStripEl = byId<HTMLElement>("videoReferenceStrip");
const bgmInput = byId<HTMLInputElement>("videoBgmInput");
const voiceInput = byId<HTMLInputElement>("videoVoiceInput");
const recordBtn = byId<HTMLButtonElement>("videoRecordBtn");
const recordPreview = byId<HTMLElement>("videoRecordPreview");
const bgmListEl = byId<HTMLElement>("videoBgmList");
const voiceListEl = byId<HTMLElement>("videoVoiceList");
const finalSectionEl = byId<HTMLElement>("videoFinalSection");

// In-memory state. We re-fetch the project detail after every mutation
// instead of trying to keep a granular cache in sync — round-trips are
// cheap on localhost and the simpler logic is easier to reason about.
let projects: VideoProject[] = [];
let configs: VideoLLMConfig[] = [];
let activeProjectID: number | null = null;
let activeProject: VideoProject | null = null;
let activeShots: VideoShot[] = [];
let activeAssets: VideoAsset[] = [];

// MediaRecorder voice booth state.
let mediaRecorder: MediaRecorder | null = null;
let mediaStream: MediaStream | null = null;
let recordedChunks: Blob[] = [];
let recordedBlob: Blob | null = null;

function escapeHtml(input: string): string {
  return input
    .split("&").join("&amp;")
    .split("<").join("&lt;")
    .split(">").join("&gt;")
    .split('"').join("&quot;")
    .split("'").join("&#39;");
}

function statusLabel(status: string): string {
  const key = `video.status.${status}`;
  const translated = t(key);
  return translated && translated !== key ? translated : status;
}

function renderProjectList(): void {
  if (!projects.length) {
    projectListEl.innerHTML = `<div class="chat-empty">${escapeHtml(t("video.noProjects") || "No projects yet")}</div>`;
    return;
  }
  projectListEl.innerHTML = projects
    .map((project) => {
      const cls = project.id === activeProjectID ? "video-project-row active" : "video-project-row";
      const title = escapeHtml(project.title || t("video.untitled") || "Untitled");
      return `
        <button class="${cls}" data-project-id="${project.id}" type="button">
          <div class="video-project-row-title">${title}</div>
          <div class="video-project-row-meta">${escapeHtml(statusLabel(project.status))}</div>
        </button>
      `;
    })
    .join("");
}

function renderConfigSelect(): void {
  const options = [`<option value="">${escapeHtml(t("video.followProjectDefault") || "Follow project default")}</option>`];
  for (const cfg of configs) {
    const selected = activeProject?.default_llm_config_id === cfg.id ? "selected" : "";
    const owner = cfg.shared && !cfg.has_api_key ? "(shared, no key)" : "";
    options.push(`<option value="${cfg.id}" ${selected}>${escapeHtml(cfg.name)} · ${escapeHtml(cfg.model)} ${owner}</option>`);
  }
  projectConfigSelect.innerHTML = options.join("");
}

function renderProjectDetail(): void {
  if (!activeProject) {
    studioEmpty.hidden = false;
    studioProject.hidden = true;
    return;
  }
  studioEmpty.hidden = true;
  studioProject.hidden = false;
  projectTitleInput.value = activeProject.title || "";
  projectStatusEl.textContent = statusLabel(activeProject.status);
  projectStatusEl.className = `video-status-pill status-${activeProject.status}`;
  renderConfigSelect();
  renderShotList();
  renderAssets();
  renderFinal();
}

function renderShotList(): void {
  if (!activeShots.length) {
    shotListEl.innerHTML = `<div class="chat-empty">${escapeHtml(t("video.noShots") || "No shots yet")}</div>`;
    return;
  }
  const sorted = [...activeShots].sort((a, b) => a.ord - b.ord || a.id - b.id);
  shotListEl.innerHTML = sorted
    .map((shot, idx) => {
      const status = escapeHtml(statusLabel(shot.status));
      const promptValue = escapeHtml(shot.prompt || "");
      const ratio = escapeHtml(shot.ratio || "9:16");
      const dur = shot.duration || 10;
      const errorBlock = shot.error_message
        ? `<div class="video-shot-error">${escapeHtml(shot.error_message)}</div>`
        : "";
      // Cache-bust video and poster URLs whenever the row changes so a
      // regenerate (which keeps the deterministic filename) doesn't serve
      // stale bytes out of the browser cache. updated_at bumps on every
      // status change, so it's a natural version cookie.
      const cacheBust = shot.updated_at ? `?v=${encodeURIComponent(shot.updated_at)}` : "";
      const videoSrc = shot.video_url ? `${shot.video_url}${cacheBust}` : "";
      const posterSrc = shot.poster_url ? `${shot.poster_url}${cacheBust}` : "";
      // preload="none" + poster avoids browsers fetching MP4 bytes until
      // the user actually presses play. The cached poster is generated
      // server-side via ffmpeg right after the shot lands in storage.
      const posterAttr = posterSrc ? ` poster="${escapeHtml(posterSrc)}"` : "";
      const playerBlock = videoSrc
        ? `<video class="video-shot-preview" src="${escapeHtml(videoSrc)}" controls preload="none"${posterAttr}></video>`
        : `<div class="video-shot-placeholder">${escapeHtml(t("video.notReady") || "Not ready yet")}</div>`;
      const submitable = shot.status === "pending" || shot.status === "failed";
      const submitLabel = shot.status === "failed"
        ? escapeHtml(t("video.retry") || "Retry")
        : escapeHtml(t("video.submit") || "Submit");
      // Regenerate is the "I want to tweak the prompt and try again" path
      // for shots already in the can. Only show it on succeeded — queued
      // and running already have a request in flight; pending/failed use
      // the Submit/Retry chip path.
      const regenBtn = shot.status === "succeeded"
        ? `<button class="btn-chip" data-shot-action="regenerate" type="button">${escapeHtml(t("video.regenerate") || "Regenerate")}</button>`
        : "";
      const downloadBtn = shot.video_url
        ? `<a class="btn-chip" href="${escapeHtml(videoSrc)}" download>${escapeHtml(t("video.download") || "Download")}</a>`
        : "";
      const captureBtn = shot.video_url
        ? `<button class="btn-chip" data-shot-action="capture-frame" type="button" title="${escapeHtml(t("video.useThisFrameHint") || "Pause the video at the desired frame, then click")}">${escapeHtml(t("video.useThisFrame") || "📸 Use this frame")}</button>`
        : "";
      return `
        <article class="video-shot-card" data-shot-id="${shot.id}">
          <header class="video-shot-head">
            <span class="video-shot-index">#${idx + 1}</span>
            <span class="video-status-pill status-${shot.status}">${status}</span>
            <input class="input video-shot-ratio" type="text" data-shot-field="ratio" value="${ratio}" />
            <input class="input video-shot-duration" type="number" min="1" max="60" data-shot-field="duration" value="${dur}" />
            <div class="video-shot-actions">
              ${submitable ? `<button class="btn-chip" data-shot-action="submit" type="button">${submitLabel}</button>` : ""}
              ${regenBtn}
              ${downloadBtn}
              ${captureBtn}
              <button class="btn-chip" data-shot-action="duplicate" type="button">${escapeHtml(t("video.duplicate") || "Duplicate")}</button>
              <button class="btn-chip" data-shot-action="delete" type="button">${escapeHtml(t("common.delete") || "Delete")}</button>
            </div>
          </header>
          <textarea class="input video-shot-prompt" data-shot-field="prompt" rows="3">${promptValue}</textarea>
          ${errorBlock}
          ${playerBlock}
        </article>
      `;
    })
    .join("");
}

function renderAssets(): void {
  const bgm = activeAssets.filter((a) => a.kind === "audio_bgm");
  const voice = activeAssets.filter((a) => a.kind === "voiceover");
  bgmListEl.innerHTML = renderAssetGroup(bgm, "audio_bgm");
  voiceListEl.innerHTML = renderAssetGroup(voice, "voiceover");
  renderReferenceStrip();
}

function renderReferenceStrip(): void {
  const refs = activeAssets.filter((a) => a.kind === "character_reference");
  if (!refs.length) {
    referenceStripEl.hidden = true;
    referenceStripEl.innerHTML = "";
    return;
  }
  referenceStripEl.hidden = false;
  // Latest-first so the one currently used by submissions is leftmost.
  const ordered = [...refs].sort((a, b) => (a.created_at < b.created_at ? 1 : -1));
  referenceStripEl.innerHTML = `
    <div class="video-reference-head">
      <span class="badge">${escapeHtml(t("video.characterReferences") || "Character references")}</span>
      <span class="video-reference-hint">${escapeHtml(t("video.characterReferenceHint") || "Latest used as first frame on submit")}</span>
    </div>
    <div class="video-reference-thumbs">
      ${ordered
        .map(
          (asset, idx) => `
        <div class="video-reference-thumb${idx === 0 ? " is-active" : ""}" data-asset-id="${asset.id}">
          <img src="${escapeHtml(asset.url)}" alt="character reference" />
          <button class="video-reference-delete" type="button" data-asset-action="delete" title="${escapeHtml(t("common.delete") || "Delete")}">×</button>
        </div>
      `,
        )
        .join("")}
    </div>
  `;
}

function renderAssetGroup(assets: VideoAsset[], kind: "audio_bgm" | "voiceover"): string {
  if (!assets.length) {
    const empty = kind === "audio_bgm"
      ? t("video.noBgm") || "No background music yet"
      : t("video.noVoiceover") || "No voiceover yet";
    return `<div class="chat-empty">${escapeHtml(empty)}</div>`;
  }
  return assets
    .map((asset) => {
      const volumeField = kind === "audio_bgm" ? "bgm_volume" : "voice_volume";
      const volumeValue = kind === "audio_bgm" ? asset.bgm_volume : asset.voice_volume;
      return `
        <div class="video-asset-card" data-asset-id="${asset.id}">
          <div class="video-asset-meta">
            <div class="video-asset-name">${escapeHtml(asset.file_name || "audio")}</div>
            <audio src="${escapeHtml(asset.url)}" controls preload="metadata"></audio>
          </div>
          <div class="video-asset-tools">
            <label class="video-asset-volume">
              <span>${escapeHtml(t("video.volume") || "Volume")}</span>
              <input type="range" min="0" max="2" step="0.05" value="${volumeValue}" data-asset-field="${volumeField}" />
              <span class="video-asset-volume-value">${volumeValue.toFixed(2)}</span>
            </label>
            <button class="btn-chip" data-asset-action="delete" type="button">${escapeHtml(t("common.delete") || "Delete")}</button>
          </div>
        </div>
      `;
    })
    .join("");
}

function renderFinal(): void {
  if (!activeProject) {
    finalSectionEl.innerHTML = "";
    return;
  }
  if (activeProject.status === "rendered" && activeProject.final_video_url) {
    finalSectionEl.innerHTML = `
      <video class="video-final-preview" src="${escapeHtml(activeProject.final_video_url)}" controls preload="metadata"></video>
      <a class="btn-chip" href="${escapeHtml(activeProject.final_video_url)}" download>${escapeHtml(t("video.download") || "Download")}</a>
    `;
    return;
  }
  if (activeProject.status === "rendering") {
    finalSectionEl.innerHTML = `<div class="chat-empty">${escapeHtml(t("video.rendering") || "Rendering...")}</div>`;
    return;
  }
  if (activeProject.status === "failed" && activeProject.final_render_error) {
    finalSectionEl.innerHTML = `<div class="video-render-error">${escapeHtml(activeProject.final_render_error)}</div>`;
    return;
  }
  finalSectionEl.innerHTML = `<div class="chat-empty">${escapeHtml(t("video.notRendered") || "Not rendered yet")}</div>`;
}

async function loadProjects(): Promise<void> {
  const { response, data } = await fetchVideoProjects();
  if (!response.ok) {
    projectListEl.innerHTML = `<div class="chat-empty">${escapeHtml(data.error || t("common.networkError") || "Network error")}</div>`;
    return;
  }
  projects = data.projects || [];
  renderProjectList();
}

async function loadConfigs(): Promise<void> {
  const { response, data } = await fetchVideoLLMConfigs();
  if (!response.ok) {
    configs = [];
    return;
  }
  configs = data.configs || [];
}

async function openProject(id: number): Promise<void> {
  activeProjectID = id;
  renderProjectList();
  const { response, data } = await fetchVideoProject(id);
  if (!response.ok || !data.project) {
    activeProjectID = null;
    activeProject = null;
    renderProjectDetail();
    return;
  }
  activeProject = data.project;
  activeShots = data.shots || [];
  activeAssets = data.assets || [];
  renderProjectDetail();
}

async function refreshActiveProject(): Promise<void> {
  if (activeProjectID == null) return;
  await openProject(activeProjectID);
}

newProjectBtn.addEventListener("click", async () => {
  const title = window.prompt(t("video.promptNewProjectTitle") || "Project title", t("video.untitled") || "New video") || "";
  if (!title.trim()) return;
  const { response, data } = await createVideoProject(title.trim());
  if (!response.ok || !data.project) {
    alert(data.error || "Failed to create project");
    return;
  }
  await loadProjects();
  await openProject(data.project.id);
});

projectListEl.addEventListener("click", (event) => {
  const target = event.target as HTMLElement;
  const row = target.closest<HTMLElement>(".video-project-row");
  if (!row) return;
  const id = Number(row.dataset.projectId);
  if (!id) return;
  void openProject(id);
});

projectTitleInput.addEventListener("change", async () => {
  if (!activeProject) return;
  const title = projectTitleInput.value.trim();
  if (!title) return;
  const { response, data } = await updateVideoProject(activeProject.id, { title });
  if (!response.ok || !data.project) {
    alert(data.error || "Failed to rename");
    return;
  }
  activeProject = data.project;
  await loadProjects();
});

projectConfigSelect.addEventListener("change", async () => {
  if (!activeProject) return;
  const raw = projectConfigSelect.value;
  const id = raw ? Number(raw) : null;
  const { response, data } = await updateVideoProject(activeProject.id, { default_llm_config_id: id });
  if (!response.ok || !data.project) {
    alert(data.error || "Failed to update");
    return;
  }
  activeProject = data.project;
});

addShotBtn.addEventListener("click", async () => {
  if (!activeProject) return;
  const { response, data } = await createVideoShot(activeProject.id, { prompt: "" });
  if (!response.ok || !data.shot) {
    alert(data.error || "Failed to add shot");
    return;
  }
  await refreshActiveProject();
});

// Bulk-import: split a pasted script on blank lines, create one shot per
// paragraph. Mirrors the user's original openclaw.sh PROMPTS=( ... ) array.
function parseBulkScript(raw: string): string[] {
  return raw
    .split(/\r?\n\s*\r?\n/) // blank-line separator (one or more)
    .map((chunk) => chunk.trim())
    .filter(Boolean);
}

function refreshBulkPreview(): void {
  const prompts = parseBulkScript(bulkImportText.value);
  if (prompts.length === 0) {
    bulkImportPreview.textContent = "";
    bulkImportConfirmBtn.disabled = true;
    return;
  }
  const tpl = t("video.bulkImportPreview") || "{{count}} shots will be created";
  bulkImportPreview.textContent = tpl.replace("{{count}}", String(prompts.length));
  bulkImportConfirmBtn.disabled = false;
}

bulkImportBtn.addEventListener("click", () => {
  if (!activeProject) return;
  bulkImportPanel.hidden = false;
  bulkImportText.value = "";
  refreshBulkPreview();
  bulkImportText.focus();
});

bulkImportCancelBtn.addEventListener("click", () => {
  bulkImportPanel.hidden = true;
  bulkImportText.value = "";
});

bulkImportText.addEventListener("input", refreshBulkPreview);

bulkImportConfirmBtn.addEventListener("click", async () => {
  if (!activeProject) return;
  const prompts = parseBulkScript(bulkImportText.value);
  if (prompts.length === 0) return;
  bulkImportConfirmBtn.disabled = true;
  bulkImportCancelBtn.disabled = true;
  let createdCount = 0;
  let failedCount = 0;
  // Create sequentially so the server-side ord assignment stays correct
  // (each createVideoShot reads the current max+1). Concurrent creates
  // could race and produce duplicate ord values.
  for (const prompt of prompts) {
    try {
      const { response, data } = await createVideoShot(activeProject.id, { prompt });
      if (!response.ok || !data.shot) {
        failedCount++;
        // eslint-disable-next-line no-console
        console.warn("bulk import: failed to create shot", data.error);
        continue;
      }
      createdCount++;
    } catch {
      failedCount++;
    }
  }
  bulkImportConfirmBtn.disabled = false;
  bulkImportCancelBtn.disabled = false;
  bulkImportPanel.hidden = true;
  bulkImportText.value = "";
  if (failedCount > 0) {
    const tpl = t("video.bulkImportPartial") || "Imported {{ok}} of {{total}} shots ({{fail}} failed)";
    alert(tpl.replace("{{ok}}", String(createdCount)).replace("{{total}}", String(prompts.length)).replace("{{fail}}", String(failedCount)));
  }
  await refreshActiveProject();
});

submitAllBtn.addEventListener("click", async () => {
  if (!activeProject) return;
  submitAllBtn.disabled = true;
  // Backend is async (202 + WS broadcasts), so we don't block on the
  // network round-trip — the click feedback is enough. Optimistically flip
  // the returned shot_ids to "queued" in the local state so the user sees
  // movement instantly even before the first WS event arrives.
  try {
    const { response, data } = await submitAllVideoShots(activeProject.id);
    if (!response.ok) {
      alert(data.error || "Submit-all failed");
      return;
    }
    if (data.shot_ids?.length) {
      const queued = new Set(data.shot_ids);
      activeShots = activeShots.map((shot) => (queued.has(shot.id) ? { ...shot, status: "queued" as const } : shot));
      renderShotList();
    }
  } finally {
    submitAllBtn.disabled = false;
  }
});

downloadAllBtn.addEventListener("click", () => {
  if (!activeProject) return;
  // Plain redirect — the auth cookie rides along, the response sets
  // Content-Disposition so the browser saves rather than navigates.
  window.location.href = `/api/video-projects/${activeProject.id}/shots.zip`;
});

renderBtn.addEventListener("click", async () => {
  if (!activeProject) return;
  const response = await renderVideoProject(activeProject.id);
  if (!response.ok) {
    const data = await response.json().catch(() => ({}));
    alert(data?.error || "Render failed");
    return;
  }
  await refreshActiveProject();
});

deleteProjectBtn.addEventListener("click", async () => {
  if (!activeProject) return;
  if (!window.confirm(t("video.confirmDeleteProject") || "Delete this project? This cannot be undone.")) return;
  const response = await deleteVideoProject(activeProject.id);
  if (!response.ok) {
    alert("Delete failed");
    return;
  }
  activeProject = null;
  activeProjectID = null;
  activeShots = [];
  activeAssets = [];
  renderProjectDetail();
  await loadProjects();
});

shotListEl.addEventListener("change", async (event) => {
  if (!activeProject) return;
  const target = event.target as HTMLElement;
  const card = target.closest<HTMLElement>(".video-shot-card");
  if (!card) return;
  const shotID = Number(card.dataset.shotId);
  if (!shotID) return;
  const field = (target as HTMLInputElement | HTMLTextAreaElement).dataset?.shotField;
  if (!field) return;
  const value = (target as HTMLInputElement | HTMLTextAreaElement).value;
  const body: { prompt?: string; ratio?: string; duration?: number } = {};
  if (field === "prompt") body.prompt = value;
  if (field === "ratio") body.ratio = value;
  if (field === "duration") body.duration = Number(value) || 10;
  await updateVideoShot(activeProject.id, shotID, body);
});

shotListEl.addEventListener("click", async (event) => {
  if (!activeProject) return;
  const target = event.target as HTMLElement;
  const card = target.closest<HTMLElement>(".video-shot-card");
  if (!card) return;
  const shotID = Number(card.dataset.shotId);
  if (!shotID) return;
  const action = target.dataset.shotAction;
  if (action === "submit") {
    const shot = activeShots.find((s) => s.id === shotID);
    const wasFailed = shot?.status === "failed";
    const { response, data } = wasFailed
      ? await retryVideoShot(activeProject.id, shotID)
      : await submitVideoShot(activeProject.id, shotID);
    if (!response.ok) {
      alert(data.error || "Submit failed");
    }
    await refreshActiveProject();
  } else if (action === "capture-frame") {
    const videoEl = card.querySelector<HTMLVideoElement>("video.video-shot-preview");
    if (!videoEl) {
      alert("Video not available yet");
      return;
    }
    const ms = (videoEl.currentTime || 0) * 1000;
    const { response, data } = await extractCharacterFrame(activeProject.id, shotID, ms);
    if (!response.ok || !data.asset) {
      alert(data.error || "Capture failed");
      return;
    }
    activeAssets = [data.asset, ...activeAssets];
    renderAssets();
  } else if (action === "duplicate") {
    const { response, data } = await duplicateVideoShot(activeProject.id, shotID);
    if (!response.ok) {
      alert(data.error || "Duplicate failed");
      return;
    }
    await refreshActiveProject();
  } else if (action === "regenerate") {
    if (!window.confirm(t("video.confirmRegenerate") || "Regenerating will replace the current video. Continue?")) return;
    // Optimistic flip so the shot card immediately shows "queued" state
    // and the stale player vanishes — WS broadcast reconciles on real
    // status arrival. Reuses the existing /shots/:id/submit endpoint;
    // the backend's markVideoShotResubmitted clears video_url + poster_url
    // server-side too.
    const idx = activeShots.findIndex((s) => s.id === shotID);
    if (idx !== -1) {
      const next = activeShots.slice();
      next[idx] = { ...next[idx], status: "queued", video_url: undefined, poster_url: undefined, error_message: undefined };
      activeShots = next;
      renderShotList();
    }
    const { response, data } = await submitVideoShot(activeProject.id, shotID);
    if (!response.ok) {
      alert(data.error || "Regenerate failed");
      // Recover authoritative state on failure (the row may still be
      // succeeded if the provider rejected the new request).
      await refreshActiveProject();
    }
  } else if (action === "delete") {
    if (!window.confirm(t("video.confirmDeleteShot") || "Delete this shot?")) return;
    const response = await deleteVideoShot(activeProject.id, shotID);
    if (!response.ok) {
      alert("Delete failed");
    }
    await refreshActiveProject();
  }
});

bgmInput.addEventListener("change", async () => {
  if (!activeProject || !bgmInput.files?.[0]) return;
  const file = bgmInput.files[0];
  const { response, data } = await uploadVideoAsset(activeProject.id, "audio_bgm", file);
  if (!response.ok) {
    alert(data.error || "Upload failed");
  }
  bgmInput.value = "";
  await refreshActiveProject();
});

voiceInput.addEventListener("change", async () => {
  if (!activeProject || !voiceInput.files?.[0]) return;
  const file = voiceInput.files[0];
  const { response, data } = await uploadVideoAsset(activeProject.id, "voiceover", file);
  if (!response.ok) {
    alert(data.error || "Upload failed");
  }
  voiceInput.value = "";
  await refreshActiveProject();
});

// MediaRecorder voice booth. Browsers expose webm/opus on Chromium and
// mp4/aac on Safari; ffmpeg consumes both natively, so we just upload the
// blob with whatever container the browser chose.
async function startRecording(): Promise<void> {
  if (!navigator.mediaDevices?.getUserMedia) {
    alert("MediaRecorder not supported in this browser");
    return;
  }
  try {
    mediaStream = await navigator.mediaDevices.getUserMedia({ audio: true });
  } catch (err) {
    alert("Microphone permission denied");
    return;
  }
  recordedChunks = [];
  recordedBlob = null;
  const mimeCandidates = ["audio/webm;codecs=opus", "audio/mp4", "audio/webm"];
  let mime = "";
  for (const candidate of mimeCandidates) {
    if (typeof MediaRecorder !== "undefined" && MediaRecorder.isTypeSupported(candidate)) {
      mime = candidate;
      break;
    }
  }
  mediaRecorder = mime ? new MediaRecorder(mediaStream, { mimeType: mime }) : new MediaRecorder(mediaStream);
  mediaRecorder.ondataavailable = (event) => {
    if (event.data && event.data.size > 0) {
      recordedChunks.push(event.data);
    }
  };
  mediaRecorder.onstop = () => {
    recordedBlob = new Blob(recordedChunks, { type: mediaRecorder?.mimeType || "audio/webm" });
    renderRecordPreview();
  };
  mediaRecorder.start();
  recordBtn.textContent = t("video.stopRecord") || "Stop";
  recordBtn.classList.add("is-recording");
}

function stopRecording(): void {
  if (mediaRecorder && mediaRecorder.state !== "inactive") {
    mediaRecorder.stop();
  }
  if (mediaStream) {
    for (const track of mediaStream.getTracks()) {
      track.stop();
    }
    mediaStream = null;
  }
  recordBtn.textContent = t("video.record") || "Record";
  recordBtn.classList.remove("is-recording");
}

function renderRecordPreview(): void {
  if (!recordedBlob) {
    recordPreview.hidden = true;
    recordPreview.innerHTML = "";
    return;
  }
  const url = URL.createObjectURL(recordedBlob);
  recordPreview.hidden = false;
  recordPreview.innerHTML = `
    <audio src="${url}" controls></audio>
    <button class="btn-chip" id="videoRecordUploadBtn" type="button">${escapeHtml(t("video.useThisRecording") || "Upload")}</button>
    <button class="btn-chip" id="videoRecordDiscardBtn" type="button">${escapeHtml(t("video.discardRecording") || "Discard")}</button>
  `;
  byId<HTMLButtonElement>("videoRecordUploadBtn").addEventListener("click", async () => {
    if (!activeProject || !recordedBlob) return;
    const ext = recordedBlob.type.includes("mp4") ? ".mp4" : ".webm";
    const file = new File([recordedBlob], `voiceover-${Date.now()}${ext}`, { type: recordedBlob.type });
    const { response, data } = await uploadVideoAsset(activeProject.id, "voiceover", file);
    if (!response.ok) {
      alert(data.error || "Upload failed");
      return;
    }
    recordedBlob = null;
    URL.revokeObjectURL(url);
    renderRecordPreview();
    await refreshActiveProject();
  });
  byId<HTMLButtonElement>("videoRecordDiscardBtn").addEventListener("click", () => {
    recordedBlob = null;
    URL.revokeObjectURL(url);
    renderRecordPreview();
  });
}

recordBtn.addEventListener("click", () => {
  if (mediaRecorder && mediaRecorder.state === "recording") {
    stopRecording();
  } else {
    void startRecording();
  }
});

const assetListContainer = document.querySelector(".video-audio-grid");
assetListContainer?.addEventListener("change", async (event) => {
  if (!activeProject) return;
  const target = event.target as HTMLInputElement;
  const card = target.closest<HTMLElement>(".video-asset-card");
  if (!card) return;
  const assetID = Number(card.dataset.assetId);
  if (!assetID) return;
  const field = target.dataset.assetField;
  if (!field) return;
  const value = Number(target.value);
  if (!Number.isFinite(value)) return;
  const body: { bgm_volume?: number; voice_volume?: number } = {};
  if (field === "bgm_volume") body.bgm_volume = value;
  if (field === "voice_volume") body.voice_volume = value;
  await updateVideoAssetVolumes(activeProject.id, assetID, body);
  // Update the volume label in place without a full re-render so the slider
  // doesn't jump during drag.
  const label = card.querySelector<HTMLElement>(".video-asset-volume-value");
  if (label) label.textContent = value.toFixed(2);
});

assetListContainer?.addEventListener("click", async (event) => {
  if (!activeProject) return;
  const target = event.target as HTMLElement;
  const card = target.closest<HTMLElement>(".video-asset-card");
  if (!card) return;
  if (target.dataset.assetAction !== "delete") return;
  const assetID = Number(card.dataset.assetId);
  if (!assetID) return;
  if (!window.confirm(t("video.confirmDeleteAsset") || "Delete this audio?")) return;
  const response = await deleteVideoAsset(activeProject.id, assetID);
  if (!response.ok) {
    alert("Delete failed");
    return;
  }
  await refreshActiveProject();
});

referenceStripEl.addEventListener("click", async (event) => {
  if (!activeProject) return;
  const target = event.target as HTMLElement;
  if (target.dataset.assetAction !== "delete") return;
  const thumb = target.closest<HTMLElement>(".video-reference-thumb");
  if (!thumb) return;
  const assetID = Number(thumb.dataset.assetId);
  if (!assetID) return;
  const response = await deleteVideoAsset(activeProject.id, assetID);
  if (!response.ok) {
    alert("Delete failed");
    return;
  }
  activeAssets = activeAssets.filter((a) => a.id !== assetID);
  renderAssets();
});

// ---- WebSocket: multiplex on the existing chat connection ---------------

function connectWebSocket(): void {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(`${protocol}//${window.location.host}/ws/chat`);
  ws.addEventListener("message", (event) => {
    let payload: VideoStudioEventPayload | null = null;
    try {
      payload = JSON.parse(event.data);
    } catch {
      return;
    }
    if (!payload || payload.type !== "video_project") return;
    if (!activeProjectID || payload.project_id !== activeProjectID) return;
    if (payload.kind === "shot_status" && payload.payload?.shot_id) {
      const idx = activeShots.findIndex((s) => s.id === payload!.payload!.shot_id);
      if (idx === -1) {
        // New shot status for an unknown row — refetch to avoid drift.
        void refreshActiveProject();
        return;
      }
      const next = { ...activeShots[idx] };
      if (payload.payload.status) next.status = payload.payload.status as VideoShot["status"];
      if (payload.payload.video_url !== undefined) next.video_url = payload.payload.video_url;
      if (payload.payload.error_message !== undefined) next.error_message = payload.payload.error_message;
      activeShots = activeShots.slice();
      activeShots[idx] = next;
      renderShotList();
      return;
    }
    if (payload.kind === "render_status" && activeProject) {
      const next = { ...activeProject };
      if (payload.payload?.status) next.status = payload.payload.status as VideoProject["status"];
      if (payload.payload?.final_video_url !== undefined) next.final_video_url = payload.payload.final_video_url;
      if (payload.payload?.final_render_error !== undefined) next.final_render_error = payload.payload.final_render_error;
      activeProject = next;
      renderProjectDetail();
      // Update the sidebar pill too.
      const matchIdx = projects.findIndex((p) => p.id === next.id);
      if (matchIdx >= 0) {
        const updated = projects.slice();
        updated[matchIdx] = next;
        projects = updated;
        renderProjectList();
      }
    }
  });
  ws.addEventListener("close", () => {
    // Cheap reconnect; chat WS uses the same endpoint and there's no
    // server-side cap on connections per user, so two reconnect attempts
    // (one from chat, one from here) are tolerated.
    setTimeout(connectWebSocket, 3000);
  });
}

// ---- init ----------------------------------------------------------------

async function init(): Promise<void> {
  await hydrateSiteBrand();
  const { response, data } = await fetchCurrentUser();
  if (!response.ok) {
    window.location.href = "/login.html";
    return;
  }
  renderSidebarFoot(data);
  await loadConfigs();
  await loadProjects();
  if (projects.length > 0) {
    await openProject(projects[0].id);
  }
  connectWebSocket();
}

document.getElementById("logoutBtn")?.addEventListener("click", async () => {
  try { await logout(); } finally { window.location.replace("/login.html"); }
});

void init();
