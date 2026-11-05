"use strict";

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

const state = {
  config: null,
  builds: [],
  detailId: null,
  logAbort: null,
};

async function api(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: { ...(opts.body ? { "Content-Type": "application/json" } : {}), ...(opts.headers || {}) },
  });
  if (res.status === 204) return null;
  const body = res.headers.get("Content-Type")?.includes("application/json") ? await res.json() : await res.text();
  if (!res.ok) {
    const err = new Error(body?.error || body || res.statusText);
    err.fields = body?.fields || {};
    throw err;
  }
  return body;
}

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === "class") node.className = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c == null) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

/* ---------- formatting ---------- */

function fmtTime(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

function fmtDuration(b) {
  if (!b.startedAt) return "—";
  const end = b.completedAt ? new Date(b.completedAt) : new Date();
  let s = Math.max(0, Math.round((end - new Date(b.startedAt)) / 1000));
  const h = Math.floor(s / 3600); s -= h * 3600;
  const m = Math.floor(s / 60); s -= m * 60;
  return h ? `${h}h ${m}m` : m ? `${m}m ${s}s` : `${s}s`;
}

function osLabel(id) {
  return state.config?.catalog.operatingSystems.find((o) => o.id === id)?.label || id;
}

function statusPill(phase) {
  return el("span", { class: `pill pill-${(phase || "pending").toLowerCase()}` }, phase || "Pending");
}

/* ---------- config & form ---------- */

async function loadConfig() {
  const cfg = await api("/api/config");
  state.config = cfg;

  const env = $("#env");
  env.replaceChildren(
    ...(cfg.prismEndpoint ? [el("dt", {}, "Prism Central"), el("dd", {}, cfg.prismEndpoint)] : []),
    el("dt", {}, "Namespace"), el("dd", {}, cfg.namespace),
  );

  const osWrap = $("#os-options");
  osWrap.replaceChildren(...cfg.catalog.operatingSystems.map((o, i) =>
    el("label", { class: "os-card" },
      el("input", { type: "radio", name: "os", value: o.id, checked: i === 0 }),
      el("span", { class: "os-name" }, o.label,
        cfg.fips.enabled && o.fipsCapable ? el("span", { class: "tag", title: "FIPS 140 capable" }, "FIPS") : null,
        cfg.stig.enabled && o.stigCapable ? el("span", { class: "tag", title: "DISA STIG capable" }, "STIG") : null),
      el("span", { class: "os-id" }, o.id),
    )));
  $("#fips-option").classList.toggle("hidden", !cfg.fips.enabled);
  $("#stig-option").classList.toggle("hidden", !cfg.stig.enabled);

  $("#k8s-versions").replaceChildren(...cfg.catalog.kubernetesVersions.map((v) => el("option", { value: v })));

  const f = $("#build-form");
  const d = cfg.defaults;
  f.kubernetesVersion.value = d.kubernetesVersion || "";
  f.clusterName.value = d.clusterName || "";
  f.subnetName.value = d.subnetName || "";
  f.cpus.value = d.cpus;
  f.memoryMB.value = d.memoryMB;
  f.diskSizeGB.value = d.diskSizeGB;
  f.httpProxy.value = d.httpProxy || "";
  f.httpsProxy.value = d.httpsProxy || "";
  f.noProxy.value = d.noProxy || "";

  $("#credentials-section").classList.toggle("hidden", !cfg.features.allowCredentialOverride);
  $("#extra-vars-field").classList.toggle("hidden", !cfg.features.allowExtraVars);
  onFormChange();
}

function selectedOS() {
  const id = $("#build-form").querySelector("input[name=os]:checked")?.value;
  return state.config.catalog.operatingSystems.find((o) => o.id === id);
}

function onFormChange() {
  const f = $("#build-form");
  const os = selectedOS();
  if (!os) return;
  const stigBox = f.stig;
  stigBox.disabled = !os.stigCapable;
  if (!os.stigCapable) stigBox.checked = false;
  $("#stig-hint").textContent = os.stigCapable
    ? "Applies the DISA STIG profile (SCAP Security Guide) with documented Kubernetes exceptions, scans the result and keeps the report in the image. Requires FIPS."
    : `Not available for ${os.label}. DISA STIG builds are supported on RHEL 9 and Rocky Linux 9.`;
  // FIPS mode is a STIG requirement.
  const fipsBox = f.fips;
  fipsBox.disabled = !os.fipsCapable || stigBox.checked;
  if (stigBox.checked) fipsBox.checked = true;
  if (!os.fipsCapable) fipsBox.checked = false;
  $("#fips-hint").textContent = os.fipsCapable
    ? "FIPS kernel mode and crypto policy, with FIPS builds of Kubernetes, containerd and runc."
    : `Not available for ${os.label}. FIPS builds are supported on RHEL 9 and Rocky Linux 9.`;
  const d = state.config.defaults;
  const version = f.kubernetesVersion.value.trim().replace(/^v/, "") || "…";
  $("#image-name-default").textContent = (d.imageNameTemplate || "")
    .replaceAll("{prefix}", d.imageNamePrefix || "")
    .replaceAll("{os}", os.imageNameOS || os.id)
    .replaceAll("{version}", version)
    .replaceAll("{fips}", fipsSelected() || stigSelected() ? "-fips" : "")
    .replaceAll("{stig}", stigSelected() ? "-stig" : "")
    .replaceAll("{timestamp}", "<timestamp>");
  $("#source-image-required").classList.toggle("hidden", !os.requiresImageURL);
  // OSes without a usable default image must pick a Prism image or a URL.
  $("#source-mode-default").classList.toggle("hidden", os.requiresImageURL);
  if (os.requiresImageURL && sourceMode() === "default") {
    f.querySelector("input[name=sourceMode][value=prism]").checked = true;
  }
  $("#source-image-hint").textContent = os.defaultImageURL
    ? `Downloads ${os.defaultImageURL}`
    : `Downloads the image_url in packer/nutanix/${os.id}.json`;
  const mode = sourceMode();
  $$("[data-source-mode]").forEach((n) => n.classList.toggle("hidden", n.dataset.sourceMode !== mode));
  $("#rhsm-fields").classList.toggle("hidden", !os.requiresRHSM);
}

function fipsSelected() {
  const os = selectedOS();
  return Boolean(state.config.fips.enabled && os?.fipsCapable && $("#build-form").fips.checked);
}

function stigSelected() {
  const os = selectedOS();
  return Boolean(state.config.stig.enabled && os?.stigCapable && $("#build-form").stig.checked);
}

function sourceMode() {
  return $("#build-form").querySelector("input[name=sourceMode]:checked")?.value || "default";
}

function parseExtraVars(text) {
  const out = {};
  for (const [i, raw] of text.split("\n").entries()) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const eq = line.indexOf("=");
    if (eq < 1) throw Object.assign(new Error(`Line ${i + 1}: expected name=value`), { fields: { extraVars: `line ${i + 1}: expected name=value` } });
    out[line.slice(0, eq).trim()] = line.slice(eq + 1).trim();
  }
  return out;
}

function collectRequest() {
  const f = $("#build-form");
  const num = (v) => (v === "" ? 0 : Number(v));
  const req = {
    os: selectedOS()?.id || "",
    kubernetesVersion: f.kubernetesVersion.value,
    imageName: f.imageName.value,
    clusterName: f.clusterName.value,
    subnetName: f.subnetName.value,
    cpus: num(f.cpus.value),
    memoryMB: num(f.memoryMB.value),
    diskSizeGB: num(f.diskSizeGB.value),
    sourceImageURL: sourceMode() === "url" ? f.sourceImageURL.value : "",
    sourceImage: sourceMode() === "prism" ? f.sourceImage.value : "",
    httpProxy: f.httpProxy.value,
    httpsProxy: f.httpsProxy.value,
    noProxy: f.noProxy.value,
    extraDebs: f.extraDebs.value,
    extraRpms: f.extraRpms.value,
  };
  if (fipsSelected()) req.fips = true;
  if (stigSelected()) req.stig = req.fips = true;
  if (state.config.features.allowExtraVars) {
    const vars = parseExtraVars(f.extraVars.value);
    if (Object.keys(vars).length) req.extraVars = vars;
  }
  if (state.config.features.allowCredentialOverride) {
    const c = {
      endpoint: f["cred.endpoint"].value,
      port: f["cred.port"].value,
      username: f["cred.username"].value,
      password: f["cred.password"].value,
      rhsmUser: f["cred.rhsmUser"].value,
      rhsmPass: f["cred.rhsmPass"].value,
    };
    if (f["cred.insecure"].checked) c.insecure = true;
    for (const k of Object.keys(c)) if (c[k] === "") delete c[k];
    if (Object.keys(c).length) req.credentials = c;
  }
  return req;
}

function showErrors(err) {
  $$("[data-error-for]").forEach((n) => (n.textContent = ""));
  $$(".invalid").forEach((n) => n.classList.remove("invalid"));
  $("#form-error").textContent = "";
  if (!err) return;
  const fields = err.fields || {};
  let firstInvalid = null;
  for (const [k, msg] of Object.entries(fields)) {
    const slot = $(`[data-error-for="${CSS.escape(k)}"]`);
    if (slot) slot.textContent = msg;
    const inputName = k.startsWith("credentials.") ? "cred." + k.slice(12) : k;
    const input = $("#build-form").elements[inputName];
    if (input instanceof HTMLElement) {
      input.classList.add("invalid");
      firstInvalid ??= input;
    }
    if (k.startsWith("credentials")) $("#credentials-section").open = true;
    if (["httpProxy", "httpsProxy", "noProxy", "extraDebs", "extraRpms", "extraVars"].includes(k)) {
      $("#extra-vars-field").closest("details").open = true;
    }
  }
  $("#form-error").textContent = Object.keys(fields).length ? "Fix the highlighted fields." : err.message;
  firstInvalid?.focus();
}

async function onSubmit(ev) {
  ev.preventDefault();
  const btn = $("#submit");
  showErrors(null);
  let req;
  try {
    req = collectRequest();
  } catch (e) {
    showErrors(e);
    return;
  }
  btn.disabled = true;
  btn.textContent = "Starting…";
  try {
    const b = await api("/api/builds", { method: "POST", body: JSON.stringify(req) });
    const f = $("#build-form");
    f["cred.password"].value = "";
    f["cred.rhsmPass"].value = "";
    await refreshBuilds();
    openDetail(b.id);
  } catch (e) {
    showErrors(e);
  } finally {
    btn.disabled = false;
    btn.textContent = "Start build";
  }
}

/* ---------- builds list ---------- */

async function refreshBuilds() {
  try {
    state.builds = await api("/api/builds");
  } catch (e) {
    console.error(e);
    return;
  }
  const body = $("#builds-body");
  body.replaceChildren(...state.builds.map((b) =>
    el("tr", { tabindex: "0", class: b.id === state.detailId ? "active" : null,
               onclick: () => openDetail(b.id),
               onkeydown: (e) => { if (e.key === "Enter") openDetail(b.id); } },
      el("td", {}, statusPill(b.phase)),
      el("td", { class: "mono" }, b.imageName,
        b.request?.fips ? el("span", { class: "tag" }, "FIPS") : null,
        b.request?.stig ? el("span", { class: "tag" }, "STIG") : null),
      el("td", {}, osLabel(b.os)),
      el("td", { class: "mono" }, b.request?.kubernetesVersion || "—"),
      el("td", {}, fmtTime(b.startedAt || b.createdAt)),
      el("td", {}, fmtDuration(b)),
    )));
  $("#builds-empty").classList.toggle("hidden", state.builds.length > 0);
  if (state.detailId) renderDetailHeader(state.builds.find((b) => b.id === state.detailId));
}

/* ---------- detail & logs ---------- */

function renderDetailHeader(b) {
  if (!b) return;
  $("#detail-title").replaceChildren(statusPill(b.phase), " ", b.imageName);
  $("#detail-sub").textContent = `${b.name} · ${osLabel(b.os)}`;
  const r = b.request || {};
  const rows = [
    ["Kubernetes", r.kubernetesVersion],
    ["FIPS 140", r.fips ? "Yes" : null],
    ["DISA STIG", r.stig ? "Yes. Scan report and exceptions in the image at /usr/share/image-builder/stig; summary at the end of the log" : null],
    ["Cluster", r.clusterName],
    ["Subnet", r.subnetName],
    ["VM size", r.cpus ? `${r.cpus} vCPU · ${r.memoryMB} MB · ${r.diskSizeGB} GB` : null],
    ["Source image", r.sourceImage ? `Prism Central: ${r.sourceImage}` : r.sourceImageURL || "repo default"],
    ["Credentials", r.credentials ? `override (${r.credentials.username || "default user"})` : "service default"],
    ["Created", fmtTime(b.createdAt)],
    ["Duration", fmtDuration(b)],
    ["Message", b.message],
  ].filter(([, v]) => v);
  $("#detail-meta").replaceChildren(...rows.flatMap(([k, v]) => [el("dt", {}, k), el("dd", {}, v)]));
  const done = b.phase === "Succeeded" || b.phase === "Failed";
  const cancel = $("#detail-cancel");
  cancel.textContent = done ? "Delete record" : "Cancel build";
  cancel.disabled = b.phase === "Cancelling";
  $("#detail-next").classList.toggle("hidden", b.phase !== "Succeeded");
  const k8s = (r.kubernetesVersion || "").replace(/^v/, "");
  const fips = state.config.fips;
  const lines = r.fips
    ? [
        `--vm-image ${b.imageName}`,
        `--kubernetes-version v${k8s}+${fips.buildMetadata}`,
        `--kubernetes-image-repository ${fips.imageRepository}`,
        `--etcd-version <etcd-version>+${fips.buildMetadata}`,
        `--etcd-image-repository ${fips.imageRepository}`,
      ]
    : [`--vm-image ${b.imageName}`, `--kubernetes-version ${k8s}`];
  $("#detail-next-cmd").textContent =
    ["nkp create cluster nutanix", ...lines.map((l) => `  ${l}`), "  …"].join(" \\\n") +
    (r.fips ? '\n\n# <etcd-version>: the etcd version from the "FIPS etcd image"\n# line in the build log, e.g. 3.6.8' : "");
}

async function openDetail(id) {
  stopLogs();
  state.detailId = id;
  const dlg = $("#detail");
  $("#log").textContent = "";
  $("#detail-vars").textContent = "";
  if (!dlg.open) dlg.showModal();
  try {
    const b = await api(`/api/builds/${id}`);
    renderDetailHeader(b);
    $("#detail-vars").textContent = b.packerVars || "(not available)";
  } catch (e) {
    $("#log").textContent = `Failed to load build: ${e.message}`;
    return;
  }
  streamLogs(id);
  $$("#builds-body tr").forEach((tr, i) => tr.classList.toggle("active", state.builds[i]?.id === id));
}

function stopLogs() {
  state.logAbort?.abort();
  state.logAbort = null;
}

async function streamLogs(id) {
  const ctrl = new AbortController();
  state.logAbort = ctrl;
  const log = $("#log");
  const append = (text) => {
    log.append(text);
    if ($("#autoscroll").checked) log.scrollTop = log.scrollHeight;
  };
  let attempt = 0;
  while (!ctrl.signal.aborted && state.detailId === id) {
    log.textContent = "";
    try {
      const res = await fetch(`/api/builds/${id}/logs?follow=1`, { signal: ctrl.signal });
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body.error || res.statusText);
      }
      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        append(value);
      }
      // Stream ended: either the container exited or a proxy timed out the
      // connection. Reconnect (replaying the full log) only if still running.
      const b = await api(`/api/builds/${id}`).catch(() => null);
      if (!b || !["Pending", "Running"].includes(b.phase)) return;
      await new Promise((r) => setTimeout(r, 2000));
      continue;
    } catch (e) {
      if (ctrl.signal.aborted) return;
      const b = state.builds.find((x) => x.id === id);
      if (b && (b.phase === "Succeeded" || b.phase === "Failed") && attempt > 0) {
        append(`\n[webui] ${e.message}\n`);
        return;
      }
      log.textContent = `Waiting for build pod… (${e.message})`;
      attempt++;
      await new Promise((r) => setTimeout(r, 3000));
    }
  }
}

function closeDetail() {
  stopLogs();
  state.detailId = null;
  $("#detail").close();
  $$("#builds-body tr.active").forEach((tr) => tr.classList.remove("active"));
}

async function onCancel() {
  const id = state.detailId;
  const b = state.builds.find((x) => x.id === id);
  const running = b && !["Succeeded", "Failed"].includes(b.phase);
  const msg = running
    ? "Cancel this build? Packer will be interrupted and will try to delete the build VM in Prism Central. A partially built image is not kept."
    : "Delete this build record and its logs? The image in Prism Central is not affected.";
  if (!confirm(msg)) return;
  try {
    await api(`/api/builds/${id}`, { method: "DELETE" });
  } catch (e) {
    alert(e.message);
  }
  if (!running) closeDetail();
  refreshBuilds();
}

/* ---------- boot ---------- */

document.addEventListener("DOMContentLoaded", async () => {
  const f = $("#build-form");
  f.addEventListener("submit", onSubmit);
  f.addEventListener("input", onFormChange);
  f.addEventListener("change", onFormChange);
  $("#refresh").addEventListener("click", refreshBuilds);
  $("#detail-close").addEventListener("click", closeDetail);
  $("#detail").addEventListener("close", () => { stopLogs(); state.detailId = null; });
  $("#detail-cancel").addEventListener("click", onCancel);

  try {
    await loadConfig();
  } catch (e) {
    $("#form-error").textContent = `Could not load configuration: ${e.message}`;
    $("#submit").disabled = true;
  }
  await refreshBuilds();
  setInterval(refreshBuilds, 5000);
});
