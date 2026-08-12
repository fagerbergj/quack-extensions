// quack usage dashboard - vanilla JS, no build step, no CDN. Charts are
// hand-rolled inline SVG (bars + a line/area chart): the dataset here is a
// handful of series over a handful of ranges, nowhere near where a ~40KB
// charting library (uPlot et al.) earns its weight.

// Single source of truth for every metric/label string this page queries.
// MIRRORS quack's internal/otelobs token/cost instruments
// (gen_ai.client.token.usage, gen_ai.client.cost) - that contract is landing
// in a parallel PR and may drift, so this block is the one place to fix on
// integration. OTel counters surface to Prometheus as
// "<dotted_name_with_underscores>_total".
const METRICS = {
  tokenUsage: "gen_ai_client_token_usage_total",
  cost: "gen_ai_client_cost_total",
  labels: {
    // the contract names this gen_ai_request_model, but semconv-derived
    // OTel->Prometheus exporters sometimes surface it unqualified as
    // "model" - group by both and resolve whichever is non-empty (see
    // firstLabel below) rather than guessing which one lands.
    model: "gen_ai_request_model",
    modelFallback: "model",
    source: "source",
    agent: "agent",
    user: "user",
    tokenType: "token_type",
  },
  tokenTypeCached: "cached",
  tokenTypeInput: "input",
};

const RANGES = { "1h": 3600, "24h": 86400, "7d": 604800, "30d": 2592000 };
const DEFAULT_RANGE_KEY = "24h";

const EMPTY_TOKENS_MSG = "no data — quack's token metrics require v0.30+.";
const EMPTY_COST_MSG = "no data — quack's token metrics require v0.30+ and a configured price table for cost.";

function rangeKeyFor(seconds) {
  for (const [key, value] of Object.entries(RANGES)) {
    if (value === seconds) return key;
  }
  return DEFAULT_RANGE_KEY;
}

// --- Prometheus fetch helpers, via this extension's own proxy only. ---

async function promQuery(query, time) {
  const params = new URLSearchParams({ query, time: String(time) });
  return promFetch("api/query?" + params.toString());
}

async function promQueryRange(query, start, end, step) {
  const params = new URLSearchParams({ query, start: String(start), end: String(end), step: String(step) });
  return promFetch("api/query_range?" + params.toString());
}

async function promFetch(path) {
  let resp;
  try {
    resp = await fetch(path, { headers: { Accept: "application/json" } });
  } catch (err) {
    return { networkError: String(err) };
  }
  let body;
  try {
    body = await resp.json();
  } catch (err) {
    return { networkError: "invalid response from usage API" };
  }
  return body;
}

// --- Query builders. rangeKey is a Prometheus duration string ("1h" etc),
// valid directly as a range-vector selector duration. ---

function tokensByDimQuery(dimLabel, rangeKey) {
  return `sum by (${dimLabel}) (increase(${METRICS.tokenUsage}[${rangeKey}]))`;
}

function tokensByModelQuery(rangeKey) {
  return `sum by (${METRICS.labels.model}, ${METRICS.labels.modelFallback}) (increase(${METRICS.tokenUsage}[${rangeKey}]))`;
}

function tokenTypeTotalsQuery(rangeKey) {
  return `sum by (${METRICS.labels.tokenType}) (increase(${METRICS.tokenUsage}[${rangeKey}]))`;
}

function costTotalQuery(rangeKey) {
  return `sum(increase(${METRICS.cost}[${rangeKey}]))`;
}

function costSeriesQuery(stepSeconds) {
  return `sum(increase(${METRICS.cost}[${stepSeconds}s]))`;
}

// firstLabel picks the first non-empty label value off a Prometheus metric
// object, in preference order - see the METRICS.labels.model comment.
function firstLabel(metric, ...names) {
  for (const name of names) {
    if (metric[name]) return metric[name];
  }
  return "(unknown)";
}

// --- Rendering: small DOM/SVG helpers, no dependency. ---

function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
}

function showEmpty(el, msg) {
  clear(el);
  const p = document.createElement("div");
  p.className = "chart-empty";
  p.textContent = msg;
  el.appendChild(p);
}

function showError(el, msg) {
  clear(el);
  const p = document.createElement("div");
  p.className = "chart-error";
  p.textContent = "error: " + msg;
  el.appendChild(p);
}

// promResultOrStatus classifies a Prometheus API response into
// {ok:true, series} or {ok:false, empty, message}.
function promResultOrStatus(resp) {
  if (resp.networkError) {
    return { ok: false, empty: false, message: resp.networkError };
  }
  if (resp.status !== "success") {
    return { ok: false, empty: false, message: resp.error || "prometheus returned an error" };
  }
  const series = (resp.data && resp.data.result) || [];
  if (series.length === 0) {
    return { ok: false, empty: true, message: "" };
  }
  return { ok: true, series };
}

function renderBarChart(el, items, emptyMsg) {
  // items: [{label, value}], already resolved and sorted desc by caller.
  if (items.length === 0) {
    showEmpty(el, emptyMsg);
    return;
  }
  clear(el);
  const max = Math.max(...items.map((i) => i.value), 1);
  for (const item of items.slice(0, 10)) {
    const row = document.createElement("div");
    row.className = "bar-row";

    const label = document.createElement("div");
    label.className = "bar-label";
    label.title = item.label;
    label.textContent = item.label;

    const track = document.createElement("div");
    track.className = "bar-track";
    const fill = document.createElement("div");
    fill.className = "bar-fill";
    fill.style.width = Math.max(2, (item.value / max) * 100) + "%";
    track.appendChild(fill);

    const value = document.createElement("div");
    value.className = "bar-value";
    value.textContent = formatNumber(item.value);

    row.appendChild(label);
    row.appendChild(track);
    row.appendChild(value);
    el.appendChild(row);
  }
}

function formatNumber(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(2) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(2) + "K";
  return Math.round(n * 100) / 100 + "";
}

const SVG_NS = "http://www.w3.org/2000/svg";

// renderLineChart draws a simple line+area chart over [{t, v}], t in unix
// seconds. Pure SVG, no scaling library.
function renderLineChart(el, points) {
  clear(el);
  if (points.length === 0) return;

  const width = 600;
  const height = 140;
  const padding = 8;

  const values = points.map((p) => p.v);
  const minV = Math.min(0, ...values);
  const maxV = Math.max(...values, 0.0001);
  const minT = points[0].t;
  const maxT = points[points.length - 1].t || minT + 1;
  const spanT = Math.max(1, maxT - minT);

  const x = (t) => padding + ((t - minT) / spanT) * (width - 2 * padding);
  const y = (v) => height - padding - ((v - minV) / (maxV - minV || 1)) * (height - 2 * padding);

  const linePoints = points.map((p) => `${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join(" ");
  const areaPoints = `${x(minT).toFixed(1)},${y(0).toFixed(1)} ${linePoints} ${x(maxT).toFixed(1)},${y(0).toFixed(1)}`;

  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
  svg.setAttribute("class", "chart-svg");
  svg.setAttribute("preserveAspectRatio", "none");

  const area = document.createElementNS(SVG_NS, "polygon");
  area.setAttribute("points", areaPoints);
  area.setAttribute("fill", "#3b6ea5");
  area.setAttribute("fill-opacity", "0.15");
  area.setAttribute("stroke", "none");
  svg.appendChild(area);

  const line = document.createElementNS(SVG_NS, "polyline");
  line.setAttribute("points", linePoints);
  line.setAttribute("fill", "none");
  line.setAttribute("stroke", "#3b6ea5");
  line.setAttribute("stroke-width", "2");
  svg.appendChild(line);

  el.appendChild(svg);
}

function renderStat(el, label, value) {
  const tile = document.createElement("div");
  tile.className = "stat-tile";
  tile.textContent = value;
  const sub = document.createElement("div");
  sub.className = "stat-sub";
  sub.textContent = label;
  el.appendChild(tile);
  el.appendChild(sub);
}

// --- Panel loaders ---

async function loadTokensByDim(panelEl, dimLabel, rangeKey, buildQuery) {
  const query = (buildQuery || tokensByDimQuery)(dimLabel, rangeKey);
  const resp = await promQuery(query, currentNowSeconds());
  const status = promResultOrStatus(resp);
  if (!status.ok) {
    if (status.empty) return showEmpty(panelEl, EMPTY_TOKENS_MSG);
    return showError(panelEl, status.message);
  }
  const items = status.series
    .map((s) => ({
      label: dimLabel === "__model__" ? firstLabel(s.metric, METRICS.labels.model, METRICS.labels.modelFallback) : s.metric[dimLabel] || "(unknown)",
      value: Number(s.value[1]),
    }))
    .filter((i) => i.value > 0)
    .sort((a, b) => b.value - a.value);
  renderBarChart(panelEl, items, EMPTY_TOKENS_MSG);
}

async function loadCacheRate(panelEl, rangeKey) {
  const resp = await promQuery(tokenTypeTotalsQuery(rangeKey), currentNowSeconds());
  const status = promResultOrStatus(resp);
  if (!status.ok) {
    if (status.empty) return showEmpty(panelEl, EMPTY_TOKENS_MSG);
    return showError(panelEl, status.message);
  }
  let cached = 0;
  let input = 0;
  for (const s of status.series) {
    const t = s.metric[METRICS.labels.tokenType];
    const v = Number(s.value[1]);
    if (t === METRICS.tokenTypeCached) cached = v;
    if (t === METRICS.tokenTypeInput) input = v;
  }
  clear(panelEl);
  if (cached + input === 0) {
    showEmpty(panelEl, EMPTY_TOKENS_MSG);
    return;
  }
  const rate = (cached / (cached + input)) * 100;
  renderStat(panelEl, `${formatNumber(cached)} cached / ${formatNumber(input)} input tokens`, rate.toFixed(1) + "%");
}

async function loadCost(panelEl, rangeKey, start, end, stepSeconds) {
  const [totalResp, seriesResp] = await Promise.all([
    promQuery(costTotalQuery(rangeKey), currentNowSeconds()),
    promQueryRange(costSeriesQuery(stepSeconds), start, end, stepSeconds),
  ]);

  const totalStatus = promResultOrStatus(totalResp);
  const seriesStatus = promResultOrStatus(seriesResp);

  clear(panelEl);
  if (!totalStatus.ok && !totalStatus.empty) {
    return showError(panelEl, totalStatus.message);
  }
  if (!seriesStatus.ok && !seriesStatus.empty) {
    return showError(panelEl, seriesStatus.message);
  }
  if ((totalStatus.empty || !totalStatus.ok) && (seriesStatus.empty || !seriesStatus.ok)) {
    return showEmpty(panelEl, EMPTY_COST_MSG);
  }

  const total = totalStatus.ok ? Number(totalStatus.series[0].value[1]) : 0;
  const totalTile = document.createElement("div");
  renderStat(totalTile, `total for selected range`, "$" + total.toFixed(4));
  panelEl.appendChild(totalTile);

  const chartWrap = document.createElement("div");
  panelEl.appendChild(chartWrap);
  if (seriesStatus.ok) {
    const points = seriesStatus.series[0].values.map(([t, v]) => ({ t, v: Number(v) }));
    renderLineChart(chartWrap, points);
  } else {
    showEmpty(chartWrap, EMPTY_COST_MSG);
  }
}

function currentNowSeconds() {
  return Math.floor(Date.now() / 1000);
}

function stepFor(rangeSeconds) {
  return Math.max(15, Math.floor(rangeSeconds / 60));
}

async function refreshAll(rangeKey) {
  const rangeSeconds = RANGES[rangeKey];
  const end = currentNowSeconds();
  const start = end - rangeSeconds;
  const step = stepFor(rangeSeconds);

  const panels = {
    "tokens-model": () => loadTokensByDim(panelEl("tokens-model"), "__model__", rangeKey, () => tokensByModelQuery(rangeKey)),
    "tokens-source": () => loadTokensByDim(panelEl("tokens-source"), METRICS.labels.source, rangeKey),
    "tokens-agent": () => loadTokensByDim(panelEl("tokens-agent"), METRICS.labels.agent, rangeKey),
    "tokens-user": () => loadTokensByDim(panelEl("tokens-user"), METRICS.labels.user, rangeKey),
    "cache-rate": () => loadCacheRate(panelEl("cache-rate"), rangeKey),
    cost: () => loadCost(panelEl("cost"), rangeKey, start, end, step),
  };

  await Promise.all(Object.values(panels).map((fn) => fn().catch((err) => console.error("usage: panel load failed", err))));
}

function panelEl(name) {
  return document.querySelector(`[data-panel="${name}"]`);
}

function initRangeSelect() {
  const cfg = window.__USAGE_CONFIG__ || {};
  const initialKey = rangeKeyFor(cfg.defaultRangeSeconds);
  const buttons = document.querySelectorAll("#range-select button");
  buttons.forEach((btn) => {
    btn.setAttribute("aria-pressed", String(btn.dataset.range === initialKey));
    btn.addEventListener("click", () => {
      buttons.forEach((b) => b.setAttribute("aria-pressed", String(b === btn)));
      refreshAll(btn.dataset.range);
    });
  });
  return initialKey;
}

document.addEventListener("DOMContentLoaded", () => {
  const initialKey = initRangeSelect();
  refreshAll(initialKey);
});
