(() => {
  "use strict";

  const state = {
    files: [],
    clipboard: [],
    filter: "all",
    connectUrl: "",
    activeXHR: null,
    sse: null,
  };

  // DOM References
  const el = {
    sseLed: document.getElementById("sse-led"),
    lanIp: document.getElementById("lan-ip"),
    deviceNum: document.getElementById("device-num"),
    btnShowQr: document.getElementById("btn-show-qr"),
    pinOverlay: document.getElementById("pin-overlay"),
    pinForm: document.getElementById("pin-form"),
    pinInput: document.getElementById("pin-input"),
    pinError: document.getElementById("pin-error"),
    workspace: document.getElementById("workspace"),
    dropzone: document.getElementById("dropzone"),
    fileInput: document.getElementById("file-input"),
    transferBox: document.getElementById("transfer-box"),
    transferFilename: document.getElementById("transfer-filename"),
    transferPct: document.getElementById("transfer-pct"),
    progressFill: document.getElementById("progress-fill"),
    metricSpeed: document.getElementById("metric-speed"),
    metricBytes: document.getElementById("metric-bytes"),
    metricEta: document.getElementById("metric-eta"),
    btnCancelUpload: document.getElementById("btn-cancel-upload"),
    filesCount: document.getElementById("files-count"),
    filesList: document.getElementById("files-list"),
    filterTabs: document.querySelectorAll(".tab-btn"),
    clipForm: document.getElementById("clip-form"),
    clipInput: document.getElementById("clip-input"),
    clipFeed: document.getElementById("clip-feed"),
    qrModal: document.getElementById("qr-modal"),
    qrImage: document.getElementById("qr-image"),
    qrUrlText: document.getElementById("qr-url-text"),
    btnCloseQr: document.getElementById("btn-close-qr"),
    btnCopyUrl: document.getElementById("btn-copy-url"),
    toast: document.getElementById("toast"),
  };

  let toastTimer = null;
  function showToast(msg) {
    el.toast.textContent = msg;
    el.toast.classList.remove("hidden");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.toast.classList.add("hidden"), 2400);
  }

  function formatBytes(bytes) {
    if (!Number.isFinite(bytes) || bytes < 0) return "0 B";
    if (bytes < 1024) return `${bytes} B`;
    const units = ["KB", "MB", "GB", "TB"];
    let val = bytes / 1024;
    let idx = 0;
    while (val >= 1024 && idx < units.length - 1) {
      val /= 1024;
      idx++;
    }
    return `${val.toFixed(1)} ${units[idx]}`;
  }

  function formatETA(seconds) {
    if (!Number.isFinite(seconds) || seconds < 0 || seconds > 86400) return "--:--";
    const s = Math.ceil(seconds);
    const mins = Math.floor(s / 60);
    const secs = s % 60;
    return `${String(mins).padStart(2, "0")}:${String(secs).padStart(2, "0")}`;
  }

  function formatTime(iso) {
    if (!iso) return "";
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return "";
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  }

  function escapeHTML(str) {
    const div = document.createElement("div");
    div.textContent = str;
    return div.innerHTML;
  }

  function linkifyText(raw) {
    const escaped = escapeHTML(raw);
    return escaped.replace(
      /(https?:\/\/[^\s<]+)/g,
      '<a href="$1" target="_blank" rel="noopener noreferrer">$1</a>'
    );
  }

  // Works reliably over non-HTTPS LAN origins (e.g. http://192.168.x.x on iOS Safari / Android Chrome).
  async function copyToClipboard(text) {
    if (navigator.clipboard && window.isSecureContext) {
      try {
        await navigator.clipboard.writeText(text);
        return true;
      } catch (_) {
        // Fall through to execCommand fallback
      }
    }
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.top = "-9999px";
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try {
      ok = document.execCommand("copy");
    } catch (_) {
      ok = false;
    }
    document.body.removeChild(ta);
    return ok;
  }

  // Bootstrap & Authentication
  async function init() {
    try {
      const res = await fetch("/api/status", { cache: "no-store" });
      const data = await res.json();

      el.lanIp.textContent = `${data.ip}:${data.port}`;
      el.deviceNum.textContent = String(data.devices || 1);
      if (data.connectUrl) {
        state.connectUrl = data.connectUrl;
      }

      if (data.pinRequired && !data.authenticated) {
        el.pinOverlay.classList.remove("hidden");
        el.workspace.classList.add("hidden");
        el.pinInput.focus();
      } else {
        unlockWorkspace();
      }
    } catch (err) {
      showToast("CANNOT REACH AIRBEAM HOST");
    }
  }

  async function unlockWorkspace() {
    el.pinOverlay.classList.add("hidden");
    el.workspace.classList.remove("hidden");

    // Refresh status to capture connectUrl after cookie auth
    try {
      const statusRes = await fetch("/api/status", { cache: "no-store" });
      const statusData = await statusRes.json();
      if (statusData.connectUrl) {
        state.connectUrl = statusData.connectUrl;
      }
    } catch (_) {}

    await Promise.all([loadFiles(), loadClipboard()]);
    connectSSE();
  }

  el.pinForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    const pin = el.pinInput.value.trim();
    if (pin.length !== 4) return;

    el.pinError.classList.add("hidden");
    try {
      const res = await fetch("/api/auth", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ pin }),
      });
      if (!res.ok) {
        el.pinError.classList.remove("hidden");
        el.pinInput.value = "";
        el.pinInput.focus();
        return;
      }
      unlockWorkspace();
    } catch (_) {
      el.pinError.textContent = "CONNECTION ERROR";
      el.pinError.classList.remove("hidden");
    }
  });

  // Auto-submit when 4 digits are entered
  el.pinInput.addEventListener("input", () => {
    el.pinInput.value = el.pinInput.value.replace(/\D/g, "").slice(0, 4);
    if (el.pinInput.value.length === 4) {
      el.pinForm.requestSubmit();
    }
  });

  // Server-Sent Events (Real-Time Sync)
  function connectSSE() {
    if (state.sse) {
      state.sse.close();
    }
    const es = new EventSource("/api/events");
    state.sse = es;

    es.onopen = () => {
      el.sseLed.classList.add("online");
    };

    es.onerror = () => {
      el.sseLed.classList.remove("online");
    };

    es.addEventListener("devices", (evt) => {
      try {
        const data = JSON.parse(evt.data);
        if (typeof data.count === "number") {
          el.deviceNum.textContent = String(data.count);
        }
      } catch (_) {}
    });

    es.addEventListener("files_updated", () => {
      loadFiles();
    });

    es.addEventListener("clipboard", (evt) => {
      try {
        const item = JSON.parse(evt.data);
        if (item && item.id && !state.clipboard.some((c) => c.id === item.id)) {
          state.clipboard.unshift(item);
          if (state.clipboard.length > 50) state.clipboard.pop();
          renderClipboard();
          showToast(`CLIPBOARD BEAMED FROM ${item.device.toUpperCase()}`);
        }
      } catch (_) {}
    });
  }

  // File Listing & Filtering
  async function loadFiles() {
    try {
      const res = await fetch("/api/files", { cache: "no-store" });
      if (!res.ok) return;
      const data = await res.json();
      state.files = Array.isArray(data.files) ? data.files : [];
      renderFiles();
    } catch (_) {}
  }

  function renderFiles() {
    const filtered = state.files.filter((f) => {
      if (state.filter === "pc") return f.source === "pc";
      if (state.filter === "phone") return f.source !== "pc";
      return true;
    });

    el.filesCount.textContent = String(state.files.length);

    if (filtered.length === 0) {
      el.filesList.innerHTML = `<div class="empty-state mono">NO FILES IN THIS VIEW</div>`;
      return;
    }

    el.filesList.innerHTML = filtered
      .map((f) => {
        const srcClass = f.source === "pc" ? "pc" : "phone";
        const srcLabel = f.source === "pc" ? "READY ON PC" : escapeHTML(f.device || "UPLOADED").toUpperCase();
        return `
          <div class="file-row">
            <div class="file-left">
              <div class="ext-badge">${escapeHTML((f.ext || "FILE").slice(0, 5))}</div>
              <div class="file-meta">
                <div class="file-name" title="${escapeHTML(f.name)}">${escapeHTML(f.name)}</div>
                <div class="file-sub mono">
                  <span class="source-tag ${srcClass}">${srcLabel}</span>
                  <span>${escapeHTML(f.sizeHuman || formatBytes(f.size))}</span>
                  <span>${formatTime(f.uploadedAt)}</span>
                </div>
              </div>
            </div>
            <a class="btn btn-emerald" href="/api/download/${encodeURIComponent(f.id)}" download>
              DOWNLOAD
            </a>
          </div>
        `;
      })
      .join("");
  }

  el.filterTabs.forEach((btn) => {
    btn.addEventListener("click", () => {
      el.filterTabs.forEach((b) => {
        b.classList.remove("active");
        b.setAttribute("aria-selected", "false");
      });
      btn.classList.add("active");
      btn.setAttribute("aria-selected", "true");
      state.filter = btn.dataset.filter || "all";
      renderFiles();
    });
  });

  // Streaming Multi-File Upload with Live MB/s & ETA Telemetry
  function uploadFiles(fileList) {
    if (!fileList || fileList.length === 0) return;

    const files = Array.from(fileList);
    const totalBytes = files.reduce((sum, f) => sum + f.size, 0);
    const label = files.length === 1 ? files[0].name : `${files.length} FILES (${files[0].name}...)`;

    const formData = new FormData();
    files.forEach((f) => formData.append("files", f, f.name));

    const xhr = new XMLHttpRequest();
    state.activeXHR = xhr;

    const startTime = performance.now();
    let lastLoaded = 0;
    let lastTime = startTime;
    let smoothedSpeed = 0;

    el.transferBox.classList.remove("hidden");
    el.progressFill.classList.remove("done");
    el.progressFill.style.width = "0%";
    el.transferFilename.textContent = label;
    el.transferPct.textContent = "0.0%";
    el.metricSpeed.textContent = "0.0 MB/s";
    el.metricBytes.textContent = `0 B / ${formatBytes(totalBytes)}`;
    el.metricEta.textContent = "--:--";
    el.btnCancelUpload.classList.remove("hidden");

    xhr.upload.addEventListener("progress", (evt) => {
      if (!evt.lengthComputable) return;
      const now = performance.now();
      const dt = (now - lastTime) / 1000;
      const totalElapsed = Math.max((now - startTime) / 1000, 0.05);

      const pct = Math.min(100, (evt.loaded / evt.total) * 100);
      el.progressFill.style.width = `${pct.toFixed(1)}%`;
      el.transferPct.textContent = `${pct.toFixed(1)}%`;
      el.metricBytes.textContent = `${formatBytes(evt.loaded)} / ${formatBytes(evt.total)}`;

      if (dt >= 0.12 || evt.loaded === evt.total) {
        const instBps = (evt.loaded - lastLoaded) / Math.max(dt, 0.05);
        const avgBps = evt.loaded / totalElapsed;
        smoothedSpeed = smoothedSpeed === 0 ? avgBps : smoothedSpeed * 0.65 + instBps * 0.35;
        const mbps = smoothedSpeed / (1024 * 1024);
        const remainingBytes = Math.max(0, evt.total - evt.loaded);
        const etaSec = smoothedSpeed > 0 ? remainingBytes / smoothedSpeed : 0;

        el.metricSpeed.textContent = `${mbps.toFixed(1)} MB/s`;
        el.metricEta.textContent = formatETA(etaSec);
        lastLoaded = evt.loaded;
        lastTime = now;
      }
    });

    xhr.addEventListener("load", () => {
      state.activeXHR = null;
      el.btnCancelUpload.classList.add("hidden");
      if (xhr.status >= 200 && xhr.status < 300) {
        el.progressFill.style.width = "100%";
        el.progressFill.classList.add("done");
        el.transferPct.textContent = "100%";
        el.metricEta.textContent = "DONE";
        showToast(`UPLOADED ${files.length} FILE${files.length > 1 ? "S" : ""}`);
        loadFiles();
        setTimeout(() => {
          if (!state.activeXHR) el.transferBox.classList.add("hidden");
        }, 3500);
      } else {
        showToast("UPLOAD FAILED");
        el.transferBox.classList.add("hidden");
      }
    });

    xhr.addEventListener("error", () => {
      state.activeXHR = null;
      showToast("NETWORK ERROR DURING UPLOAD");
      el.transferBox.classList.add("hidden");
    });

    xhr.addEventListener("abort", () => {
      state.activeXHR = null;
      showToast("UPLOAD ABORTED");
      el.transferBox.classList.add("hidden");
    });

    xhr.open("POST", "/api/upload");
    xhr.send(formData);
  }

  el.dropzone.addEventListener("click", () => el.fileInput.click());
  el.dropzone.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      el.fileInput.click();
    }
  });

  el.fileInput.addEventListener("change", () => {
    if (el.fileInput.files && el.fileInput.files.length > 0) {
      uploadFiles(el.fileInput.files);
      el.fileInput.value = "";
    }
  });

  ["dragenter", "dragover"].forEach((evtName) => {
    el.dropzone.addEventListener(evtName, (e) => {
      e.preventDefault();
      el.dropzone.classList.add("drag-over");
    });
  });

  ["dragleave", "drop"].forEach((evtName) => {
    el.dropzone.addEventListener(evtName, (e) => {
      e.preventDefault();
      el.dropzone.classList.remove("drag-over");
    });
  });

  el.dropzone.addEventListener("drop", (e) => {
    if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length > 0) {
      uploadFiles(e.dataTransfer.files);
    }
  });

  el.btnCancelUpload.addEventListener("click", () => {
    if (state.activeXHR) {
      state.activeXHR.abort();
    }
  });

  // Live 2-Way Clipboard Beam
  async function loadClipboard() {
    try {
      const res = await fetch("/api/clipboard", { cache: "no-store" });
      if (!res.ok) return;
      const data = await res.json();
      state.clipboard = Array.isArray(data.items) ? data.items : [];
      renderClipboard();
    } catch (_) {}
  }

  function renderClipboard() {
    if (state.clipboard.length === 0) {
      el.clipFeed.innerHTML = `<div class="empty-state mono">CLIPBOARD CHANNEL IDLE</div>`;
      return;
    }

    el.clipFeed.innerHTML = state.clipboard
      .map(
        (item) => `
        <div class="clip-item">
          <div class="clip-item-head">
            <span class="clip-device mono">${escapeHTML(item.device || "DEVICE").toUpperCase()} · ${formatTime(item.createdAt)}</span>
            <button type="button" class="btn btn-outline btn-copy-clip" data-id="${escapeHTML(item.id)}">COPY</button>
          </div>
          <div class="clip-text">${linkifyText(item.text)}</div>
        </div>
      `
      )
      .join("");
  }

  el.clipFeed.addEventListener("click", async (e) => {
    const btn = e.target.closest(".btn-copy-clip");
    if (!btn) return;
    const item = state.clipboard.find((c) => c.id === btn.dataset.id);
    if (!item) return;

    const ok = await copyToClipboard(item.text);
    if (ok) {
      const prev = btn.textContent;
      btn.textContent = "COPIED ✓";
      setTimeout(() => {
        btn.textContent = prev;
      }, 1500);
    } else {
      showToast("COPY BLOCKED BY BROWSER");
    }
  });

  el.clipForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    const text = el.clipInput.value.trim();
    if (!text) return;

    try {
      const res = await fetch("/api/clipboard", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ text }),
      });
      if (!res.ok) {
        showToast("FAILED TO BEAM CLIPBOARD");
        return;
      }
      el.clipInput.value = "";
    } catch (_) {
      showToast("CONNECTION ERROR");
    }
  });

  el.clipInput.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
      e.preventDefault();
      el.clipForm.requestSubmit();
    }
  });

  // QR Modal
  el.btnShowQr.addEventListener("click", () => {
    el.qrImage.src = `/api/qr.png?t=${Date.now()}`;
    el.qrUrlText.textContent = state.connectUrl || window.location.origin;
    el.qrModal.classList.remove("hidden");
  });

  el.btnCloseQr.addEventListener("click", () => {
    el.qrModal.classList.add("hidden");
  });

  el.qrModal.addEventListener("click", (e) => {
    if (e.target === el.qrModal) {
      el.qrModal.classList.add("hidden");
    }
  });

  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !el.qrModal.classList.contains("hidden")) {
      el.qrModal.classList.add("hidden");
    }
  });

  el.btnCopyUrl.addEventListener("click", async () => {
    const ok = await copyToClipboard(el.qrUrlText.textContent);
    showToast(ok ? "LAN URL COPIED" : "COPY FAILED");
  });

  init();
})();
