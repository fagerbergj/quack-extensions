// quack usage dashboard - vanilla JS, no build step, no CDN. Charts are
// hand-rolled inline SVG (stacked area / multi-line): the dataset here is a
// handful of series over a handful of ranges, nowhere near where a ~40KB
// charting library (uPlot et al.) earns its weight.

// Single source of truth for every metric/label string this page queries.
// MIRRORS quack's internal/otelobs token/cost/latency instruments
// (internal/otelobs/metrics.go) - that contract can drift, so this block is
// the one place to fix on integration. OTel counters/histograms surface to
// Prometheus as "<dotted_name_with_underscores>[_unit]_total|_bucket|_sum|_count".
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
    // Same translation hazard as model: the semconv attr gen_ai.token.type
    // lands as gen_ai_token_type (verified live 2026-08-12); keep the
    // unqualified name as fallback for other exporter configs.
    tokenType: "gen_ai_token_type",
    tokenTypeFallback: "token_type",
  },
  tokenTypeCached: "cached",
  tokenTypeInput: "input",
};

// TOKEN_TYPES is the canonical display order for the headline stacked chart
// and the cache-rate chart - fixed (not discovery-order) so "cached"/"input"
// carry the same color in both places regardless of which panel's fetch
// resolves first.
const TOKEN_TYPES = ["input", "output", "reasoning", "cached"];

// LATENCY mirrors quack.model.call.duration (internal/otelobs/metrics.go):
// a Float64Histogram, unit "s", attrs: model. Name verified against the live
// collector 2026-08-12. The presence probe below still never fakes data:
// it checks for the "_bucket" series before claiming a p50/p95/p99 chart.
const LATENCY = {
  metric: "quack_model_call_duration_seconds",
};

const RANGES = { "1h": 3600, "24h": 86400, "7d": 604800, "30d": 2592000 };
const DEFAULT_RANGE_KEY = "24h";
const STORAGE_KEY = "quack.usage.timeframe";

const EMPTY_TOKENS_MSG = "no token data in this range (token metrics need quack v0.30+ to be emitting).";
const EMPTY_COST_MSG = "no cost data — cost needs a price table (providers.<p>.models.<model>) in quack's config, v0.30+.";
const EMPTY_LATENCY_MSG = `no data — the ${LATENCY.metric}_bucket histogram isn't present (check the OTel collector's Prometheus naming translation, or quack hasn't made a model call yet).`;

// --- Step heuristic. MIRRORS usage/step.go's stepForSpan exactly (same
// table, same rounding) - the Go copy seeds the page's initial default
// range server-side; this copy drives every range change/custom pick the
// server never sees. Keep the two in lockstep by hand if the formula moves. ---

const NICE_STEPS = [15, 30, 60, 300, 900, 1800, 3600, 7200, 21600, 43200, 86400, 172800, 604800];

function stepForSpan(spanSeconds) {
  let raw = Math.floor(spanSeconds / 150);
  if (raw < 15) raw = 15;
  for (const s of NICE_STEPS) {
    if (s >= raw) return s;
  }
  return raw;
}

// --- Categorical color. Two different needs, two different mechanisms:
//
// token_type is the one dimension that appears in more than one panel (the
// headline chart and the cache-rate chart) - it gets a FIXED mapping so
// "cached" is the same color everywhere, independent of fetch order.
//
// Every other dimension (model/source/agent/user) is scoped to exactly one
// panel, so each panel gets its own fresh discovery-order assigner
// (makeColorAssigner) over the full 8-slot palette - there's no cross-panel
// value overlap to protect, and sharing one global counter across
// unrelated dimensions would just starve later panels of slots for no
// reason. A 9th+ series folds to the neutral "other" color, never a
// borrowed categorical slot. ---

const MAX_PALETTE_SLOTS = 8;

function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

const TOKEN_TYPE_SLOT = { input: 1, output: 3, reasoning: 4, cached: 6 };
function tokenTypeColor(t) {
  return cssVar(TOKEN_TYPE_SLOT[t] ? `--series-${TOKEN_TYPE_SLOT[t]}` : "--series-other");
}

function makeColorAssigner() {
  const slots = new Map();
  let next = 0;
  return function colorFor(value) {
    if (!slots.has(value)) {
      slots.set(value, next < MAX_PALETTE_SLOTS ? next++ : -1);
    }
    const slot = slots.get(value);
    return cssVar(slot < 0 ? "--series-other" : `--series-${slot + 1}`);
  };
}

// Latency percentiles get distinct FIXED colors, independent of any
// discovery-order assigner - percentile identity, not any dimension value,
// is what needs to stay visually stable here.
function latencyColor(quantileLabel) {
  if (quantileLabel === "p50") return cssVar("--series-1");
  if (quantileLabel === "p95") return cssVar("--series-2");
  return cssVar("--series-8"); // p99
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

// promResultOrStatus classifies a Prometheus API response into
// {ok:true, series} or {ok:false, empty, message}. "empty" means the whole
// query returned zero result vectors - callers with more than one series in
// play (e.g. cache-rate's cached+input) should NOT rely on this alone to
// decide emptiness; see loadCachePanel's own presence check.
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

function dotMarker(cx, cy, color) {
  const dot = document.createElementNS(SVG_NS, "circle");
  dot.setAttribute("cx", cx.toFixed(1));
  dot.setAttribute("cy", cy.toFixed(1));
  dot.setAttribute("r", "3");
  dot.setAttribute("fill", color);
  return dot;
}

function firstLabel(metric, ...names) {
  for (const name of names) {
    if (metric[name]) return metric[name];
  }
  return "(unknown)";
}

// matrixSeriesPoints turns one query_range result item into [{t,v}],
// dropping NaN samples (histogram_quantile emits "NaN" for a bucket with no
// observations in that window - that's an absent point, not a zero).
function matrixSeriesPoints(item) {
  return item.values
    .map(([t, v]) => ({ t, v: Number(v) }))
    .filter((p) => !Number.isNaN(p.v));
}

// --- Formatting ---

function formatNumber(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(2) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(2) + "K";
  return Math.round(n * 100) / 100 + "";
}

function formatMoney(n) {
  if (!Number.isFinite(n)) return "–";
  if (n >= 1000) return "$" + formatNumber(n);
  if (n < 1) return "$" + n.toFixed(4);
  return "$" + n.toFixed(2);
}

function formatPercent(n) {
  if (!Number.isFinite(n)) return "–";
  return n.toFixed(1) + "%";
}

function formatDuration(sec) {
  if (!Number.isFinite(sec)) return "–";
  if (sec < 1) return Math.round(sec * 1000) + "ms";
  if (sec < 120) return sec.toFixed(2) + "s";
  if (sec < 7200) return (sec / 60).toFixed(1) + "m";
  return (sec / 3600).toFixed(1) + "h";
}

function formatClock(t) {
  const d = new Date(t * 1000);
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

// --- Small DOM helpers ---

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

function renderStat(el, label, value, title) {
  const tile = document.createElement("div");
  tile.className = "stat-tile";
  tile.textContent = value;
  if (title) tile.title = title;
  const sub = document.createElement("div");
  sub.className = "stat-sub";
  sub.textContent = label;
  el.appendChild(tile);
  el.appendChild(sub);
}

function panelEl(name) {
  return document.querySelector(`[data-panel="${name}"]`);
}

// ============================================================
// Chart renderer: shared multi-series SVG chart with a fixed
// [start, end] x-domain (every panel uses the SAME domain from the one
// picker - never derived per-chart from its own data), a legend with a
// cheap local show/hide toggle (no cross-panel filter state), and a
// hover crosshair + tooltip.
// ============================================================

const SVG_NS = "http://www.w3.org/2000/svg";
const CHART_W = 600;
const CHART_H = 150;
const CHART_PAD = 8;

// renderSeriesChart(el, spec) where spec:
//   series: [{key, label, color, points: [{t,v}]}]
//   start, end: shared x-domain, unix seconds
//   mode: "stacked-area" | "lines"
//   emptyMsg: shown when every series has zero points
//   valueFormat: fn(v) => string (tooltip + legend), defaults to formatNumber
//   yMax: optional forced axis max (e.g. 100 for a percentage)
//   showLegend: default true when series.length > 1
function renderSeriesChart(el, spec) {
  const valueFormat = spec.valueFormat || formatNumber;
  const nonEmpty = spec.series.filter((s) => s.points.length > 0);
  clear(el);
  if (nonEmpty.length === 0) {
    showEmpty(el, spec.emptyMsg || EMPTY_TOKENS_MSG);
    return;
  }

  const hidden = new Set();
  const wrap = document.createElement("div");
  wrap.className = "chart-wrap";
  el.appendChild(wrap);

  const tooltip = document.createElement("div");
  tooltip.className = "chart-tooltip";
  tooltip.style.display = "none";
  wrap.appendChild(tooltip);

  const axis = document.createElement("div");
  axis.className = "axis-labels";
  const axisLeft = document.createElement("span");
  const axisRight = document.createElement("span");
  axisRight.textContent = formatClock(spec.end);
  axis.appendChild(axisLeft);
  axis.appendChild(axisRight);

  const legendEl = document.createElement("div");
  legendEl.className = "chart-legend";

  const showLegend = spec.showLegend !== false && spec.series.length > 1;

  function draw() {
    while (wrap.firstChild !== tooltip) wrap.removeChild(wrap.firstChild);
    const visible = spec.series.filter((s) => !hidden.has(s.key));
    axisLeft.textContent = formatClock(spec.start);

    const pointMaps = new Map();
    const tickSet = new Set();
    for (const s of visible) {
      const m = new Map();
      for (const p of s.points) {
        m.set(p.t, p.v);
        tickSet.add(p.t);
      }
      pointMaps.set(s.key, m);
    }
    const ticks = Array.from(tickSet).sort((a, b) => a - b);

    let yMax = spec.yMax;
    if (yMax === undefined) {
      if (spec.mode === "stacked-area") {
        yMax = 0.0001;
        for (const t of ticks) {
          let sum = 0;
          for (const s of visible) sum += pointMaps.get(s.key).get(t) || 0;
          if (sum > yMax) yMax = sum;
        }
      } else {
        yMax = Math.max(0.0001, ...visible.flatMap((s) => s.points.map((p) => p.v)));
      }
    }

    const x = (t) => CHART_PAD + ((t - spec.start) / Math.max(1, spec.end - spec.start)) * (CHART_W - 2 * CHART_PAD);
    const y = (v) => CHART_H - CHART_PAD - (v / yMax) * (CHART_H - 2 * CHART_PAD);

    const svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("viewBox", `0 0 ${CHART_W} ${CHART_H}`);
    svg.setAttribute("class", "chart-svg");
    svg.setAttribute("preserveAspectRatio", "none");

    // Baseline (recessive, hairline).
    const baseline = document.createElementNS(SVG_NS, "line");
    baseline.setAttribute("x1", CHART_PAD);
    baseline.setAttribute("x2", CHART_W - CHART_PAD);
    baseline.setAttribute("y1", y(0));
    baseline.setAttribute("y2", y(0));
    baseline.setAttribute("stroke", "var(--gridline)");
    baseline.setAttribute("stroke-width", "1");
    svg.appendChild(baseline);

    if (spec.mode === "stacked-area") {
      let cumBottom = ticks.map(() => 0);
      for (const s of visible) {
        const m = pointMaps.get(s.key);
        const top = ticks.map((t, i) => cumBottom[i] + (m.get(t) || 0));
        const bandPointsTop = ticks.map((t, i) => `${x(t).toFixed(1)},${y(top[i]).toFixed(1)}`);
        const bandPointsBottom = ticks
          .map((t, i) => `${x(t).toFixed(1)},${y(cumBottom[i]).toFixed(1)}`)
          .reverse();
        const band = document.createElementNS(SVG_NS, "polygon");
        band.setAttribute("points", bandPointsTop.concat(bandPointsBottom).join(" "));
        band.setAttribute("fill", s.color);
        band.setAttribute("fill-opacity", "0.55");
        band.setAttribute("stroke", "none");
        svg.appendChild(band);

        const line = document.createElementNS(SVG_NS, "polyline");
        line.setAttribute("points", bandPointsTop.join(" "));
        line.setAttribute("fill", "none");
        line.setAttribute("stroke", s.color);
        line.setAttribute("stroke-width", "2");
        svg.appendChild(line);
        // A 1-2 tick series draws no visible band/segment - mark the points.
        if (ticks.length <= 2) for (let i = 0; i < ticks.length; i++) svg.appendChild(dotMarker(x(ticks[i]), y(top[i]), s.color));

        cumBottom = top;
      }
    } else {
      for (const s of visible) {
        const pts = s.points.map((p) => `${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join(" ");
        const line = document.createElementNS(SVG_NS, "polyline");
        line.setAttribute("points", pts);
        line.setAttribute("fill", "none");
        line.setAttribute("stroke", s.color);
        line.setAttribute("stroke-width", "2");
        svg.appendChild(line);
        // A 1-2 point series has no visible segment (bit the 30d view when
        // the metric was a day old) - mark the points instead.
        if (s.points.length <= 2) for (const p of s.points) svg.appendChild(dotMarker(x(p.t), y(p.v), s.color));
      }
    }

    // Hover crosshair + tooltip.
    const guide = document.createElementNS(SVG_NS, "line");
    guide.setAttribute("y1", CHART_PAD);
    guide.setAttribute("y2", CHART_H - CHART_PAD);
    guide.setAttribute("stroke", "var(--baseline)");
    guide.setAttribute("stroke-width", "1");
    guide.style.display = "none";
    svg.appendChild(guide);

    const capture = document.createElementNS(SVG_NS, "rect");
    capture.setAttribute("x", "0");
    capture.setAttribute("y", "0");
    capture.setAttribute("width", String(CHART_W));
    capture.setAttribute("height", String(CHART_H));
    capture.setAttribute("fill", "transparent");
    svg.appendChild(capture);

    if (ticks.length > 0) {
      capture.addEventListener("mousemove", (evt) => {
        const rect = svg.getBoundingClientRect();
        const relX = ((evt.clientX - rect.left) / rect.width) * CHART_W;
        const t = spec.start + ((relX - CHART_PAD) / (CHART_W - 2 * CHART_PAD)) * (spec.end - spec.start);
        let nearest = ticks[0];
        let nearestDist = Math.abs(t - nearest);
        for (const tick of ticks) {
          const d = Math.abs(t - tick);
          if (d < nearestDist) {
            nearest = tick;
            nearestDist = d;
          }
        }
        guide.setAttribute("x1", x(nearest).toFixed(1));
        guide.setAttribute("x2", x(nearest).toFixed(1));
        guide.style.display = "";

        const lines = [`<strong>${formatClock(nearest)}</strong>`];
        for (const s of visible) {
          const v = pointMaps.get(s.key).get(nearest);
          lines.push(
            `<span style="color:${s.color}">&#9632;</span> ${s.label}: ${v === undefined ? "–" : valueFormat(v)}`
          );
        }
        tooltip.innerHTML = lines.join("<br>");
        tooltip.style.display = "";
        tooltip.style.left = Math.min(evt.clientX - rect.left + 10, rect.width - 160) + "px";
        tooltip.style.top = "0px";
      });
      capture.addEventListener("mouseleave", () => {
        guide.style.display = "none";
        tooltip.style.display = "none";
      });
    }

    wrap.insertBefore(svg, tooltip);
    wrap.insertBefore(axis, tooltip);
  }

  draw();

  if (showLegend) {
    clear(legendEl);
    for (const s of spec.series) {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "legend-item";
      btn.setAttribute("aria-pressed", "true");
      const swatch = document.createElement("span");
      swatch.className = "legend-swatch";
      swatch.style.background = s.color;
      const label = document.createElement("span");
      label.textContent = s.label;
      btn.appendChild(swatch);
      btn.appendChild(label);
      btn.addEventListener("click", () => {
        if (hidden.has(s.key)) {
          hidden.delete(s.key);
          btn.setAttribute("aria-pressed", "true");
        } else {
          hidden.add(s.key);
          btn.setAttribute("aria-pressed", "false");
        }
        draw();
      });
      legendEl.appendChild(btn);
    }
    el.appendChild(legendEl);
  }
}

// ============================================================
// Query builders
// ============================================================

function tokenTypeTotalsQuery(rangeSeconds) {
  return `sum by (${METRICS.labels.tokenType}, ${METRICS.labels.tokenTypeFallback}) (increase(${METRICS.tokenUsage}[${rangeSeconds}s]))`;
}

function tokenTypeSeriesQuery(stepSeconds) {
  return `sum by (${METRICS.labels.tokenType}, ${METRICS.labels.tokenTypeFallback}) (increase(${METRICS.tokenUsage}[${stepSeconds}s]))`;
}

function dimSeriesQuery(dimLabel, stepSeconds) {
  return `sum by (${dimLabel}) (increase(${METRICS.tokenUsage}[${stepSeconds}s]))`;
}

function modelSeriesQuery(stepSeconds) {
  return `sum by (${METRICS.labels.model}, ${METRICS.labels.modelFallback}) (increase(${METRICS.tokenUsage}[${stepSeconds}s]))`;
}

function costTotalQuery(rangeSeconds) {
  return `sum(increase(${METRICS.cost}[${rangeSeconds}s]))`;
}

function costSeriesQuery(stepSeconds) {
  return `sum(increase(${METRICS.cost}[${stepSeconds}s]))`;
}

function callCountQuery(rangeSeconds) {
  return `sum(increase(${LATENCY.metric}_count[${rangeSeconds}s]))`;
}

function latencyPresenceQuery() {
  return `count(${LATENCY.metric}_bucket)`;
}

function latencyInstantQuantileQuery(q, rangeSeconds) {
  return `histogram_quantile(${q}, sum by (le) (rate(${LATENCY.metric}_bucket[${rangeSeconds}s])))`;
}

function latencySeriesQuantileQuery(q, stepSeconds) {
  return `histogram_quantile(${q}, sum by (le) (rate(${LATENCY.metric}_bucket[${stepSeconds}s])))`;
}

// ============================================================
// Panel loaders. Each queries Prometheus and renders directly into its own
// mount point. See the "Categorical color" section above for how token_type
// stays consistent across panels while model/source/agent/user each get
// their own independent per-panel color assignment.
// ============================================================

function topKWithOther(series, k) {
  // series: [{label, points}] with a precomputed `.total`. Keeps the top k
  // by total, folds the rest into one "Other" series (dataviz rule: a 9th+
  // categorical series is never a generated hue - isOther marks it for the
  // neutral color instead of the discovery-order assigner).
  const sorted = series.slice().sort((a, b) => b.total - a.total);
  if (sorted.length <= k) return sorted.map((s) => ({ ...s, isOther: false }));
  const kept = sorted.slice(0, k).map((s) => ({ ...s, isOther: false }));
  const rest = sorted.slice(k);
  const otherPoints = new Map();
  for (const s of rest) {
    for (const p of s.points) {
      otherPoints.set(p.t, (otherPoints.get(p.t) || 0) + p.v);
    }
  }
  kept.push({
    label: `Other (${rest.length})`,
    isOther: true,
    total: rest.reduce((a, s) => a + s.total, 0),
    points: Array.from(otherPoints.entries())
      .map(([t, v]) => ({ t, v }))
      .sort((a, b) => a.t - b.t),
  });
  return kept;
}

async function loadDimSeriesPanel(name, dimLabel, start, end, step, buildQuery) {
  const el = panelEl(name);
  const query = (buildQuery || dimSeriesQuery)(dimLabel, step);
  const resp = await promQueryRange(query, start, end, step);
  const status = promResultOrStatus(resp);
  if (!status.ok) {
    if (status.empty) return showEmpty(el, EMPTY_TOKENS_MSG);
    return showError(el, status.message);
  }
  const raw = status.series.map((s) => {
    const label = dimLabel === "__model__" ? firstLabel(s.metric, METRICS.labels.model, METRICS.labels.modelFallback) : s.metric[dimLabel] || "(unknown)";
    const points = matrixSeriesPoints(s);
    return { label, points, total: points.reduce((a, p) => a + p.v, 0) };
  });
  const topped = topKWithOther(raw, MAX_PALETTE_SLOTS);
  const colorFor = makeColorAssigner();
  const series = topped.map((s) => ({
    key: s.label,
    label: s.label,
    color: s.isOther ? cssVar("--series-other") : colorFor(s.label),
    points: s.points,
  }));
  renderSeriesChart(el, { series, start, end, mode: "lines", emptyMsg: EMPTY_TOKENS_MSG });
}

async function loadHeadlineTokens(start, end, step) {
  const el = panelEl("tokens-headline");
  const resp = await promQueryRange(tokenTypeSeriesQuery(step), start, end, step);
  const status = promResultOrStatus(resp);
  if (!status.ok) {
    if (status.empty) return showEmpty(el, EMPTY_TOKENS_MSG);
    return showError(el, status.message);
  }
  const byType = new Map();
  for (const s of status.series) {
    const t = s.metric[METRICS.labels.tokenType] || s.metric[METRICS.labels.tokenTypeFallback];
    if (t) byType.set(t, matrixSeriesPoints(s));
  }
  const series = TOKEN_TYPES.filter((t) => byType.has(t)).map((t) => ({
    key: t,
    label: t,
    color: tokenTypeColor(t),
    points: byType.get(t),
  }));
  renderSeriesChart(el, { series, start, end, mode: "stacked-area", emptyMsg: EMPTY_TOKENS_MSG });
  return byType;
}

// loadCachePanel renders both the cache-rate time series and the cache
// savings tile. Empty-state fix: emptiness is decided ONLY by whether the
// "input" series is present across the range - a zero/absent "cached"
// series with real input traffic is a legitimate 0% rate, not "no data".
async function loadCachePanel(start, end, step, instantTotals) {
  const rateEl = panelEl("cache-rate");
  const savingsEl = panelEl("cache-savings");

  const resp = await promQueryRange(tokenTypeSeriesQuery(step), start, end, step);
  const status = promResultOrStatus(resp);
  let inputSeries = null;
  let cachedSeries = null;
  if (status.ok) {
    for (const s of status.series) {
      const t = s.metric[METRICS.labels.tokenType] || s.metric[METRICS.labels.tokenTypeFallback];
      if (t === METRICS.tokenTypeInput) inputSeries = matrixSeriesPoints(s);
      if (t === METRICS.tokenTypeCached) cachedSeries = matrixSeriesPoints(s);
    }
  } else if (!status.empty) {
    showError(rateEl, status.message);
  }

  if (!inputSeries || inputSeries.length === 0) {
    showEmpty(rateEl, EMPTY_TOKENS_MSG);
  } else {
    const cachedMap = new Map((cachedSeries || []).map((p) => [p.t, p.v]));
    const ratePoints = inputSeries.map((p) => {
      const cached = cachedMap.get(p.t) || 0;
      const denom = cached + p.v;
      return { t: p.t, v: denom > 0 ? (cached / denom) * 100 : 0 };
    });
    renderSeriesChart(rateEl, {
      series: [{ key: "cache-rate", label: "cache rate", color: cssVar("--series-1"), points: ratePoints }],
      start,
      end,
      mode: "lines",
      yMax: 100,
      valueFormat: formatPercent,
      showLegend: false,
    });
  }

  // Cache savings (est.): cached_tokens * (cost / input_tokens) over the
  // same instant totals used for the KPI row - true per-token price isn't
  // queryable, so this approximates it from the range's own average cost
  // per input token. Empty only when the cost series itself is absent (no
  // price table configured), independent of the rate panel's own state.
  const formula = "est. = cached_tokens × (cost ÷ input_tokens)";
  if (!instantTotals.cost.ok) {
    if (instantTotals.cost.empty) {
      showEmpty(savingsEl, EMPTY_COST_MSG);
    } else {
      showError(savingsEl, instantTotals.cost.message);
    }
    return;
  }
  const input = instantTotals.tokens.input || 0;
  const cached = instantTotals.tokens.cached || 0;
  if (input <= 0) {
    showEmpty(savingsEl, EMPTY_TOKENS_MSG);
    return;
  }
  const savings = cached * (instantTotals.cost.value / input);
  clear(savingsEl);
  renderStat(savingsEl, "cache savings (est.)", formatMoney(savings), formula);
}

async function loadCostPanel(start, end, step, instantCost) {
  const perStepEl = panelEl("cost");
  const cumulativeEl = panelEl("cost-cumulative");

  const resp = await promQueryRange(costSeriesQuery(step), start, end, step);
  const status = promResultOrStatus(resp);
  if (!status.ok) {
    if (status.empty) {
      showEmpty(perStepEl, EMPTY_COST_MSG);
      showEmpty(cumulativeEl, EMPTY_COST_MSG);
    } else {
      showError(perStepEl, status.message);
      showError(cumulativeEl, status.message);
    }
    return;
  }

  const points = matrixSeriesPoints(status.series[0]);
  renderSeriesChart(perStepEl, {
    series: [{ key: "cost", label: "cost", color: cssVar("--series-1"), points }],
    start,
    end,
    mode: "stacked-area",
    valueFormat: formatMoney,
    showLegend: false,
    emptyMsg: EMPTY_COST_MSG,
  });

  let running = 0;
  const cumPoints = points.map((p) => {
    running += p.v;
    return { t: p.t, v: running };
  });
  renderSeriesChart(cumulativeEl, {
    series: [{ key: "cost-cumulative", label: "cumulative cost", color: cssVar("--series-2"), points: cumPoints }],
    start,
    end,
    mode: "lines",
    valueFormat: formatMoney,
    showLegend: false,
    emptyMsg: EMPTY_COST_MSG,
  });

  const totalTile = document.createElement("div");
  totalTile.className = "chart-sub";
  totalTile.textContent = instantCost.ok ? `total for range: ${formatMoney(instantCost.value)}` : "";
  if (instantCost.ok) cumulativeEl.appendChild(totalTile);
}

async function loadLatencyPanel(start, end, step, latencyAvailable) {
  const el = panelEl("latency");
  if (!latencyAvailable) {
    showEmpty(el, EMPTY_LATENCY_MSG);
    return;
  }

  const quantiles = [
    { q: "0.5", key: "p50", label: "p50" },
    { q: "0.95", key: "p95", label: "p95" },
    { q: "0.99", key: "p99", label: "p99" },
  ];
  const responses = await Promise.all(
    quantiles.map((q) => promQueryRange(latencySeriesQuantileQuery(q.q, step), start, end, step))
  );

  const series = [];
  for (let i = 0; i < quantiles.length; i++) {
    const status = promResultOrStatus(responses[i]);
    if (!status.ok) continue;
    const points = matrixSeriesPoints(status.series[0]);
    if (points.length === 0) continue;
    series.push({ key: quantiles[i].key, label: quantiles[i].label, color: latencyColor(quantiles[i].key), points });
  }

  renderSeriesChart(el, {
    series,
    start,
    end,
    mode: "lines",
    valueFormat: formatDuration,
    emptyMsg: EMPTY_LATENCY_MSG,
  });
}

// --- KPI row: the compact "current instant sums" row, sticky above every
// chart, computed once per refresh from the range's own instant totals. ---

function setKPI(name, value, title) {
  const tile = document.querySelector(`[data-kpi="${name}"] .kpi-value`);
  if (!tile) return;
  tile.textContent = value;
  if (title) tile.title = title;
}

async function loadKPIRow(rangeSeconds, now, latencyAvailable, tokenTotals, costTotal) {
  const totalTokens = TOKEN_TYPES.reduce((a, t) => a + (tokenTotals[t] || 0), 0);
  setKPI("tokens", totalTokens > 0 ? formatNumber(totalTokens) : "–");
  setKPI("cost", costTotal.ok ? formatMoney(costTotal.value) : "–", costTotal.empty ? EMPTY_COST_MSG : undefined);

  if (!latencyAvailable) {
    setKPI("calls", "–", EMPTY_LATENCY_MSG);
    setKPI("latency-p95", "–", EMPTY_LATENCY_MSG);
    return;
  }
  const [callsResp, p95Resp] = await Promise.all([
    promQuery(callCountQuery(rangeSeconds), now),
    promQuery(latencyInstantQuantileQuery("0.95", rangeSeconds), now),
  ]);
  const callsStatus = promResultOrStatus(callsResp);
  const p95Status = promResultOrStatus(p95Resp);
  // A call count is an integer; increase() extrapolates fractions.
  setKPI("calls", callsStatus.ok ? formatNumber(Math.round(Number(callsStatus.series[0].value[1]))) : "–");
  setKPI("latency-p95", p95Status.ok ? formatDuration(Number(p95Status.series[0].value[1])) : "–");
}

// --- instant totals shared by the KPI row and the cache-savings tile ---

async function fetchInstantTotals(rangeSeconds, now) {
  const [tokenResp, costResp] = await Promise.all([
    promQuery(tokenTypeTotalsQuery(rangeSeconds), now),
    promQuery(costTotalQuery(rangeSeconds), now),
  ]);
  const tokenStatus = promResultOrStatus(tokenResp);
  const tokens = {};
  if (tokenStatus.ok) {
    for (const s of tokenStatus.series) {
      const t = s.metric[METRICS.labels.tokenType] || s.metric[METRICS.labels.tokenTypeFallback];
      if (t) tokens[t] = Number(s.value[1]);
    }
  }
  const costStatus = promResultOrStatus(costResp);
  const cost = costStatus.ok
    ? { ok: true, value: Number(costStatus.series[0].value[1]) }
    : { ok: false, empty: costStatus.empty, message: costStatus.message };
  return { tokens, cost };
}

// ============================================================
// Timeframe state: presets + custom range, persisted to localStorage,
// drives every panel's start/end/step (the one shared x-axis domain).
// ============================================================

function loadStoredTimeframe() {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw);
    if (parsed.mode === "preset" && RANGES[parsed.presetKey]) return parsed;
    if (parsed.mode === "custom" && typeof parsed.customFrom === "string" && typeof parsed.customTo === "string") {
      return parsed;
    }
  } catch (err) {
    // corrupt/old localStorage value - fall through to the default.
  }
  return null;
}

function saveTimeframe(tf) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(tf));
  } catch (err) {
    // localStorage unavailable (private browsing etc) - persistence is a
    // nicety, not a requirement.
  }
}

// resolveRange turns a timeframe descriptor into {start, end, step,
// rangeSeconds}, in unix seconds.
function resolveRange(tf) {
  if (tf.mode === "custom") {
    const start = Math.floor(new Date(tf.customFrom).getTime() / 1000);
    const end = Math.floor(new Date(tf.customTo).getTime() / 1000);
    const rangeSeconds = Math.max(1, end - start);
    return { start, end, rangeSeconds, step: stepForSpan(rangeSeconds) };
  }
  const rangeSeconds = RANGES[tf.presetKey] || RANGES[DEFAULT_RANGE_KEY];
  const end = Math.floor(Date.now() / 1000);
  const start = end - rangeSeconds;
  return { start, end, rangeSeconds, step: stepForSpan(rangeSeconds) };
}

async function refreshAll(tf) {
  saveTimeframe(tf);
  const { start, end, rangeSeconds, step } = resolveRange(tf);

  const now = Math.floor(Date.now() / 1000);
  const [instantTotals, presenceResp] = await Promise.all([
    fetchInstantTotals(rangeSeconds, now),
    promQuery(latencyPresenceQuery(), now),
  ]);
  const presenceStatus = promResultOrStatus(presenceResp);
  const latencyAvailable = presenceStatus.ok && Number(presenceStatus.series[0].value[1]) > 0;

  await Promise.all([
    loadHeadlineTokens(start, end, step),
    loadDimSeriesPanel("tokens-model", "__model__", start, end, step, () => modelSeriesQuery(step)),
    loadDimSeriesPanel("tokens-source", METRICS.labels.source, start, end, step),
    loadDimSeriesPanel("tokens-agent", METRICS.labels.agent, start, end, step),
    loadDimSeriesPanel("tokens-user", METRICS.labels.user, start, end, step),
    loadCachePanel(start, end, step, instantTotals),
    loadCostPanel(start, end, step, instantTotals.cost),
    loadLatencyPanel(start, end, step, latencyAvailable),
    loadKPIRow(rangeSeconds, now, latencyAvailable, instantTotals.tokens, instantTotals.cost),
  ].map((p) => p.catch((err) => console.error("usage: panel load failed", err))));
}

// --- Timeframe UI wiring ---

function toDatetimeLocalValue(epochSeconds) {
  const d = new Date(epochSeconds * 1000);
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function initTimeframeUI() {
  const cfg = window.__USAGE_CONFIG__ || {};
  const defaultKey = Object.keys(RANGES).find((k) => RANGES[k] === cfg.defaultRangeSeconds) || DEFAULT_RANGE_KEY;

  let tf = loadStoredTimeframe() || { mode: "preset", presetKey: defaultKey };

  const presetButtons = document.querySelectorAll("#range-select button");
  const fromInput = document.getElementById("range-from");
  const toInput = document.getElementById("range-to");
  const applyBtn = document.getElementById("range-apply");
  const errorEl = document.getElementById("range-error");

  function syncPressedState() {
    presetButtons.forEach((b) => {
      b.setAttribute("aria-pressed", String(tf.mode === "preset" && b.dataset.range === tf.presetKey));
    });
  }

  function seedCustomInputs() {
    const { start, end } = resolveRange(tf);
    fromInput.value = toDatetimeLocalValue(start);
    toInput.value = toDatetimeLocalValue(end);
  }

  presetButtons.forEach((btn) => {
    btn.addEventListener("click", () => {
      tf = { mode: "preset", presetKey: btn.dataset.range };
      errorEl.textContent = "";
      syncPressedState();
      seedCustomInputs();
      refreshAll(tf);
    });
  });

  applyBtn.addEventListener("click", () => {
    if (!fromInput.value || !toInput.value) {
      errorEl.textContent = "pick both a from and to date/time.";
      return;
    }
    const fromMs = new Date(fromInput.value).getTime();
    const toMs = new Date(toInput.value).getTime();
    if (Number.isNaN(fromMs) || Number.isNaN(toMs)) {
      errorEl.textContent = "invalid date/time.";
      return;
    }
    if (fromMs >= toMs) {
      errorEl.textContent = "\"from\" must be before \"to\".";
      return;
    }
    errorEl.textContent = "";
    tf = { mode: "custom", customFrom: fromInput.value, customTo: toInput.value };
    syncPressedState();
    refreshAll(tf);
  });

  syncPressedState();
  seedCustomInputs();
  return tf;
}

document.addEventListener("DOMContentLoaded", () => {
  const tf = initTimeframeUI();
  refreshAll(tf);
});
