// Browser glue for the served Sleeper page: fetches /sleeper/api/*, wires
// season/stop/talk navigation and job Run buttons, and hands data to the
// pure renderers in render.js. No build step - native ESM only.
import * as R from './render.js'

const JOBS = [
  { id: 'lineup', name: 'Start / sit', agent: 'lineup-analyst' },
  { id: 'waivers', name: 'Waiver targets', agent: 'waiver-scout' },
  { id: 'trade', name: 'Trade talks', agent: 'trade-analyst' },
  { id: 'digest', name: 'Preview / recap', agent: 'league-reporter' },
  { id: 'trends', name: 'Trends and news', agent: 'trend-scout' },
  { id: 'retro', name: 'Hindsight', agent: 'league-reporter', past: true },
]
const DRAFT_JOB = { id: 'draft', name: 'Draft analysis', agent: 'draft-analyst' }
const REVIEW_JOB = { id: 'history', name: 'Season review', agent: 'history-analyst' }

const qs = new URLSearchParams(location.search)
const state = {
  leagueID: qs.get('league_id') || '',
  seasons: null,   // /api/seasons response
  season: null,    // /api/season response for the selected season
  artifacts: null, // /api/artifacts response for the selected stop
  seasonKey: null, // selected season string
  stop: qs.get('stop') || null,
  talkIdx: 0,
  running: new Set(), // job ids currently polling
}

const $ = id => document.getElementById(id)

async function jget(url) {
  const r = await fetch(url)
  if (!r.ok) throw new Error(`${r.status} ${await r.text().catch(() => '')}`)
  return r.json()
}

function banner(msg) {
  $('banner').innerHTML = msg ? `<div class="sl-sec" style="padding:.75rem 1rem;margin-bottom:1rem;color:var(--qk-err-text)">${R.esc(msg)}</div>` : ''
}

async function loadSeasons() {
  const url = state.leagueID ? `api/seasons?league_id=${encodeURIComponent(state.leagueID)}` : 'api/seasons'
  state.seasons = await jget(url)
  state.seasonKey = state.seasons.current_season
  const cur = state.seasons.seasons.find(s => s.season === state.seasonKey)
  state.leagueID = cur.league_id
}

function leagueIDFor(seasonKey) {
  return state.seasons.seasons.find(s => s.season === seasonKey)?.league_id
}

async function loadSeason() {
  state.season = await jget(`api/season?league_id=${encodeURIComponent(state.leagueID)}`)
  if (!state.stop) state.stop = state.seasonKey === state.seasons.current_season ? String(state.season.league.week_now) : 'review'
}

async function loadArtifacts() {
  state.artifacts = await jget(`api/artifacts?league_id=${encodeURIComponent(state.leagueID)}&stop=${encodeURIComponent(state.stop)}`)
  state.talkIdx = Math.min(state.talkIdx, Math.max(0, (state.artifacts.talks || []).length - 1))
}

function isCurrentSeason() { return state.seasonKey === state.seasons.current_season }
function weekNow() { return state.season.league.week_now }
function isPastWeek() { return state.stop !== 'draft' && state.stop !== 'review' && (!isCurrentSeason() || Number(state.stop) < weekNow()) }

function jobEnvelope(job, title) {
  const a = (state.artifacts.jobs || {})[job.id]
  const found = !!a?.found
  const what = state.stop === 'draft' ? `the ${state.seasonKey} draft` : state.stop === 'review' ? `${state.seasonKey}` : `week ${state.stop}`
  return {
    job: job.id, title: title || job.name, agent: job.agent, what,
    running: state.running.has(job.id), found,
    example: !!a?.example, status: state.running.has(job.id) ? 'running' : found ? 'done' : 'not_run',
    chatHref: found ? `/chat/ext:sleeper:${state.leagueID}:${state.stop}:${job.id}` : undefined,
    data: a?.data,
  }
}

function renderHeader() {
  const s = state.season
  $('league-name').textContent = s.league.name
  $('league-sub').textContent = isCurrentSeason()
    ? `${s.me?.team ?? ''} · ${s.me ? `${s.me.wins}-${s.me.losses}` : ''} · ${s.me?.owner ?? ''} · ${s.league.season} season, week ${s.league.week_now} in progress`
    : `${s.me?.team ?? ''} · ${s.me ? `${s.me.wins}-${s.me.losses}` : ''} · ${s.league.season} season, complete`
  $('league-facts').innerHTML = (s.facts || []).map(f => `<span class="qk-chip">${R.esc(f)}</span>`).join('')
}

function renderYears() {
  $('years').innerHTML = R.renderYears(state.seasons.seasons, state.seasonKey)
}

function renderRail() {
  const found = Object.values(state.artifacts?.jobs || {}).filter(j => j.found).length
  $('rail').innerHTML = R.renderTimeline(state.stop, weekNow(), isCurrentSeason(), { [state.stop]: found })
}

function renderMenu() {
  const list = $('menu-list')
  if (state.stop === 'draft') {
    list.innerHTML = `<li class="hd">${R.esc(state.seasonKey)} draft</li><li><button data-run="draft">${isCurrentSeason() ? 'Re-grade this draft' : 'Re-run draft report card'}<span>${DRAFT_JOB.agent}</span></button></li>`
    return
  }
  if (state.stop === 'review') {
    list.innerHTML = `<li class="hd">${R.esc(state.seasonKey)} review</li><li><button data-run="history" ${isCurrentSeason() ? 'disabled' : ''}>${isCurrentSeason() ? 'Available after week 17' : 'Re-run season review'}<span>${REVIEW_JOB.agent}</span></button></li>`
    return
  }
  const past = isPastWeek()
  const jobs = JOBS.filter(j => (past ? (j.id === 'retro' || j.id === 'digest') : !j.past))
  list.innerHTML = `<li class="hd">${R.esc(state.seasonKey)} · week ${R.esc(state.stop)}</li>` + jobs.map(j => {
    const env = jobEnvelope(j)
    const label = env.running ? 'Running ' : env.found ? 'Re-run ' : 'Run '
    return `<li><button data-run="${j.id}" ${env.running ? 'disabled' : ''}>${label}${R.esc(j.name.toLowerCase())}<span>${R.esc(j.agent)}</span></button></li>`
  }).join('')
}

function renderMain() {
  const main = $('main'), side = $('side')
  if (state.stop === 'draft') {
    const env = jobEnvelope(DRAFT_JOB)
    main.innerHTML = R.renderDraftBoard(env)
    if (isCurrentSeason()) {
      side.innerHTML = R.renderDraftSide(env)
    } else {
      side.innerHTML = ''
      loadReviewHistory().then(h => { side.innerHTML = R.renderSeasonAtGlance(h) + R.renderCrossSeason(h) })
    }
    return
  }
  if (state.stop === 'review') {
    const env = jobEnvelope(REVIEW_JOB)
    main.innerHTML = R.renderReview(isCurrentSeason() ? { ...env, found: false } : env)
    side.innerHTML = isCurrentSeason() ? currentSide() : R.renderSeasonAtGlance(env.data) + R.renderCrossSeason(env.data)
    return
  }
  if (isPastWeek()) {
    main.innerHTML = R.renderRetro(jobEnvelope(JOBS[5], `Week ${state.stop} in hindsight`)) + R.renderDigest(jobEnvelope(JOBS[3], `Week ${state.stop} recap`)) + (isCurrentSeason() ? R.renderTrends(jobEnvelope(JOBS[4])) : '')
    if (isCurrentSeason()) {
      side.innerHTML = currentSide()
    } else {
      side.innerHTML = ''
      loadReviewHistory().then(h => { side.innerHTML = R.renderSeasonAtGlance(h) + R.renderCrossSeason(h) })
    }
    return
  }
  const talks = state.artifacts.talks || []
  main.innerHTML = R.renderLineup(jobEnvelope(JOBS[0], 'Start / sit')) + R.renderWaivers(jobEnvelope(JOBS[1], 'Waivers')) +
    R.renderTrade(jobEnvelope(JOBS[2], 'Trade talks'), talks, state.talkIdx) +
    R.renderDigest(jobEnvelope(JOBS[3], `Week ${state.stop} preview`)) + R.renderTrends(jobEnvelope(JOBS[4], 'Trends and news'))
  side.innerHTML = currentSide()
}

const historyCache = new Map()
async function loadReviewHistory() {
  if (historyCache.has(state.leagueID)) return historyCache.get(state.leagueID)
  const resp = await jget(`api/artifacts?league_id=${encodeURIComponent(state.leagueID)}&stop=review`)
  const data = resp.jobs?.history?.data ?? null
  historyCache.set(state.leagueID, data)
  return data
}

function currentSide() {
  const notesEnv = { title: 'Season notes', agent: 'trend-scout', found: !!state.artifacts.season_notes?.found, example: !!state.artifacts.season_notes?.example, data: state.artifacts.season_notes?.data, status: state.artifacts.season_notes?.found ? 'done' : 'not_run' }
  return R.renderStandingsSide(state.season) + R.renderSeasonNotes(notesEnv) + R.renderMovesSide(state.season)
}

function renderAll() {
  renderHeader(); renderYears(); renderRail(); renderMenu(); renderMain()
  const url = new URL(location.href)
  url.searchParams.set('league_id', state.leagueID)
  url.searchParams.set('stop', state.stop)
  history.replaceState(null, '', url)
}

async function refresh() {
  try {
    banner('')
    await loadSeason()
    await loadArtifacts()
    renderAll()
  } catch (err) {
    banner(`Could not load this league: ${err.message}`)
  }
}

async function switchSeason(season) {
  state.seasonKey = season
  state.leagueID = leagueIDFor(season)
  state.stop = season === state.seasons.current_season ? null : 'review'
  state.season = null
  await refresh()
}

async function switchStop(stop) {
  state.stop = stop
  state.talkIdx = 0
  await loadArtifacts()
  renderAll()
  window.scrollTo({ top: 0 })
}

async function runJob(job, partner) {
  $('menu').removeAttribute('open')
  const body = { league_id: state.leagueID, stop: state.stop, job, args: partner ? { partner } : undefined }
  state.running.add(job)
  renderMenu(); renderMain()
  try {
    const resp = await fetch('api/jobs', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
    if (!resp.ok) throw new Error(await resp.text())
  } catch (err) {
    state.running.delete(job)
    banner(`Could not start the run: ${err.message}`)
    renderMenu(); renderMain()
    return
  }
  pollFor(job)
}

// Dispatch is async; poll the artifacts route until the job's artifact
// shows up or a bound on attempts is hit (a stuck run must not poll forever).
function pollFor(job) {
  let attempts = 0
  const timer = setInterval(async () => {
    attempts++
    try { await loadArtifacts() } catch { /* keep polling; a transient fetch error isn't fatal */ }
    const found = job === 'trade'
      ? (state.artifacts.talks || []).some(t => t.found)
      : !!(state.artifacts.jobs || {})[job]?.found
    if (found || attempts >= 20) {
      clearInterval(timer)
      state.running.delete(job)
    }
    renderMenu(); renderMain()
  }, 3000)
}

document.addEventListener('click', e => {
  const yb = e.target.closest('[data-season]')
  if (yb) { switchSeason(yb.dataset.season); return }
  const st = e.target.closest('[data-stop]')
  if (st) { switchStop(st.dataset.stop); return }
  const rb = e.target.closest('[data-run]')
  if (rb) { runJob(rb.dataset.run, rb.dataset.partner); return }
  if (!e.target.closest('#menu')) $('menu').removeAttribute('open')
})
document.addEventListener('change', e => {
  if (e.target.id === 'talk-select') { state.talkIdx = Number(e.target.value); renderMain() }
})

async function boot() {
  try {
    await loadSeasons()
    await refresh()
  } catch (err) {
    banner(`Could not load Sleeper: ${err.message}`)
  }
}
boot()
