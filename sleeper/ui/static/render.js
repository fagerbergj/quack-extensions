// Pure rendering functions for the Sleeper extension UI: string-in,
// HTML-string-out, no DOM globals and no fetch. Shared by the served page
// (main.js) and every Storybook story so a card renders identically in
// both. Verdict/percentage semantics and markup mirror the approved
// prototype (sleeper-ui-ref/sleeper-ui.html); data comes from the
// /sleeper/api/season and /sleeper/api/artifacts JSON shapes instead of an
// inlined fixture object.

export const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]))
export const num = (n, d = 1) => Number(n ?? 0).toFixed(d)

// Sleeper's ADP sentinel for "no ADP" is 999/1000, not null - a truthy
// value, so callers must check this instead of `p.adp` directly.
export const hasADP = adp => adp != null && adp < 999

// A Sleeper player_id is all digits; a DEF's id is its team code (e.g.
// "NE") and has no player thumbnail, only a team logo.
export function avatarUrl(id) {
  if (!id) return null
  return /^\d+$/.test(id)
    ? `https://sleepercdn.com/content/nfl/players/thumb/${id}.jpg`
    : `https://sleepercdn.com/images/team_logos/nfl/${id.toLowerCase()}.png`
}

export function avatarHTML(player, sm) {
  const cls = 'av' + (sm ? ' av--sm' : '')
  const initials = (player?.name || '?').split(' ').map(w => w[0]).slice(0, 2).join('')
  if (!player?.id) return `<span class="${cls} av--i">${esc(initials)}</span>`
  const url = esc(avatarUrl(player.id))
  // No inline onerror (player names are agent-written, untrusted): a
  // delegated capture-phase 'error' listener (installOwnAvatarFallback,
  // wired once by main.js/preview.js) does the swap via data attributes.
  return `<img class="${cls}" src="${url}" alt="" loading="lazy" data-avatar-fallback="${esc(initials)}" data-avatar-class="${esc(cls)} av--i">`
}

// Installs the delegated image-fallback listener once; both the served
// page (main.js) and Storybook (preview.js) call this so avatarHTML's
// data attributes have no inline handler to carry out the swap.
export function installAvatarFallback(root = document) {
  root.addEventListener('error', e => {
    const img = e.target
    if (!(img instanceof HTMLImageElement) || img.dataset.avatarFallback == null) return
    const span = document.createElement('span')
    span.className = img.dataset.avatarClass
    span.textContent = img.dataset.avatarFallback
    img.replaceWith(span)
  }, true)
}

export function injBadge(p) {
  if (!p?.inj) return ''
  const cls = p.inj === 'IR' || p.inj === 'Out' ? 'qk-badge--err' : 'qk-badge--warn'
  return `<span class="qk-badge ${cls}">${esc(p.inj)}</span>`
}

export function personHTML(player, sm) {
  if (!player) return ''
  return `<span class="sl-p">${avatarHTML(player, sm)}<span>${esc(player.name)} <span class="sl-keep">${esc(player.pos || '')}${player.team ? ' · ' + esc(player.team) : ''}</span> ${injBadge(player)}</span></span>`
}

export function verdictBadge(verdict, confidence, why) {
  const label = verdict === 'start' ? 'Start' : verdict === 'sit' ? 'Sit' : verdict
  const okCls = verdict === 'start' ? 'qk-badge--ok' : ''
  const pct = confidence != null ? ` · ${confidence}%` : ''
  return `<span class="qk-badge hint ${okCls}" title="${esc(why || '')}" tabindex="0">${esc(label)}${pct}</span>`
}

// meta: {title, agent, status:'not_run'|'running'|'done', example, chatHref}
export function jobHead(meta) {
  let m
  if (meta.status === 'running') m = `<span class="qk-badge qk-badge--warn">Running</span><span>${esc(meta.agent || '')} · started just now</span>`
  else if (meta.status === 'done') m = `${meta.example ? '<span class="qk-badge">Example</span>' : ''}<span class="qk-badge qk-badge--ok">From chat</span><span>${esc(meta.agent || '')}</span><a href="${esc(meta.chatHref || '#')}" target="_top">Open chat</a>`
  else m = '<span class="qk-badge">Not run</span>'
  return `<div class="sl-sec__head"><h2>${esc(meta.title)}</h2><div class="sl-sec__meta">${m}</div></div>`
}

export function emptyBody(job, jobLabel, what, running) {
  if (running) return `<div class="sl-sec__body sl-empty"><span>${esc(jobLabel)} is working on ${esc(what)}. The card fills in when the run finishes.</span></div>`
  return `<div class="sl-sec__body sl-empty"><span>No ${esc(jobLabel.toLowerCase())} for ${esc(what)} yet.</span><button class="qk-btn qk-btn--primary" data-run="${esc(job)}">Run ${esc(jobLabel.toLowerCase())}</button></div>`
}

export function section(id, inner) {
  return `<section class="sl-sec" id="${esc(id)}">${inner}</section>`
}

// state: {job, title, agent, status, example, chatHref, found, data, what, running}
export function renderLineup(state) {
  const head = jobHead(state)
  if (!state.found) return section('sec-lineup', head + emptyBody(state.job, state.title, state.what, state.running))
  const d = state.data
  const rows = d.starters.map(row => `<tr title="${esc(row.why || '')}"><td>${esc(row.slot)}</td><td class="cell">${personHTML(row.player)}</td><td class="num">${num(row.proj)}</td><td>${verdictBadge(row.verdict, row.confidence, row.why)}</td></tr>`).join('')
  const benchRows = (d.bench || []).map(b => `<tr title="${esc(b.why || '')}"><td>BN</td><td class="cell">${personHTML(b.player)}</td><td class="num">${num(b.proj)}</td><td>${verdictBadge('sit', null, b.why)}</td></tr>`).join('')
  const reserve = d.reserve || []
  const irRows = reserve.map(rv => `<tr title="${esc(rv.why || '')}"><td>IR</td><td class="cell">${personHTML(rv.player)}</td><td class="num">–</td><td><span class="qk-badge">IR</span></td></tr>`).join('')
  const oppRows = (d.opponent_starters || []).map(o => `<tr><td>${esc(o.slot)}</td><td class="cell">${personHTML(o.player, true)}</td><td class="num">${num(o.proj)}</td></tr>`).join('')
  return section('sec-lineup', head + `
    <div class="sl-score"><div><b>${num(d.my_proj)}</b><span>${esc(d.team)} · ${esc(d.team_record || '')}<br>projected</span></div><div class="vs">vs</div><div><b>${num(d.opp_proj)}</b><span>${esc(d.opponent)} · ${esc(d.opponent_record || '')}<br>projected</span></div></div>
    <div class="sl-sec__body">
    <p class="sl-summary">${esc(d.summary || '')}</p>
    <div class="qk-table-wrap"><table class="qk-table"><thead><tr><th>Slot</th><th>Player</th><th class="num">Proj</th><th>Start / sit</th></tr></thead><tbody>${rows}<tr class="divider"><td colspan="4">Bench · ${(d.bench || []).length} of ${d.bench_slots || (d.bench || []).length}</td></tr>${benchRows}${irRows ? `<tr class="divider"><td colspan="4">Injured reserve · ${reserve.length} of ${d.reserve_slots || 1}, outside the roster count</td></tr>${irRows}` : ''}</tbody></table></div>
    <details class="sl-fold"><summary>${esc(d.opponent)}'s lineup · ${num(d.opp_proj)} projected</summary><div class="qk-table-wrap"><table class="qk-table"><thead><tr><th>Slot</th><th>Starter</th><th class="num">Proj</th></tr></thead><tbody>${oppRows}</tbody></table></div></details>
    <p class="sl-src">${esc(d.source_note || '')}</p></div>`)
}

export function renderWaivers(state) {
  const head = jobHead(state)
  if (!state.found) return section('sec-waivers', head + emptyBody(state.job, state.title, state.what, state.running))
  const d = state.data
  const rows = d.candidates.map(c => `<tr><td>${c.rank ?? ''}</td><td class="cell">${personHTML(c.player)}<div class="why">${esc(c.why || '')}</div></td><td class="num">${num(c.proj)}</td><td class="num">${c.owned_pct == null ? '–' : num(c.owned_pct, 0) + '%'}</td><td class="num">${c.adds_24h ? c.adds_24h.toLocaleString() : '–'}</td><td>${esc(c.drop || '')}</td></tr>`).join('')
  return section('sec-waivers', head + `<div class="sl-sec__body">
    <p class="sl-summary">${esc(d.summary || '')}</p>
    <div class="qk-table-wrap"><table class="qk-table"><thead><tr><th>#</th><th>Add and reasoning</th><th class="num">Proj</th><th class="num">Owned</th><th class="num">Adds 24h</th><th>Drop</th></tr></thead><tbody>${rows}</tbody></table></div>
    <p class="sl-src">${esc(d.source_note || '')}</p></div>`)
}

function tradeSide(label, players) {
  return `<div><div class="lbl">${esc(label)}</div>${(players || []).map(p => `<span class="sl-p">${avatarHTML(p, true)}<span>${esc(p.name)} <span class="sl-keep">${esc(p.pos || '')} · ${num(p.proj)}</span> ${injBadge(p)}</span></span>`).join('')}</div>`
}

function tradeOption(p) {
  return `<option value="${esc(p.id)}">${esc(p.name)} · ${esc(p.pos || '')} · ${num(p.proj)}</option>`
}

function tradeFinder(suggestions) {
  if (!suggestions?.length) return ''
  return `<h3 class="sl-h3">Suggested trades <span class="qk-badge">Example</span></h3><ul class="sl-finds">${suggestions.map(f => `<li><div><b>${esc(f.partner)}</b> <span class="sl-keep">${esc(f.partner_owner || '')}${f.record ? ' · ' + esc(f.record) : ''}</span><div class="sl-keep">Give ${esc(f.give.name)} (${num(f.give_proj)}) for ${esc(f.get.name)} (${num(f.get_proj)}). ${esc(f.note || '')}</div></div><button class="qk-btn" data-run="trade" data-partner="${esc(f.partner)}">Start talk</button></li>`).join('')}</ul>`
}

// talks: [{partner, found, example, status, data}]; talkIdx selects which
// one the dropdown shows.
export function renderTrade(state, talks, talkIdx) {
  const head = jobHead(state)
  const anyFound = talks.some(t => t.found)
  const suggestions = talks.find(t => t.found)?.data?.suggestions
  if (!anyFound) {
    return section('sec-trade', head + `<div class="sl-sec__body">${tradeFinder(suggestions)}<div class="sl-empty" style="margin-top:1rem"><span>No talks open. Pick a suggestion or name an offer.</span><button class="qk-btn qk-btn--primary" data-run="trade">Evaluate a trade</button></div></div>`)
  }
  const cur = talks[Math.min(talkIdx, talks.length - 1)]
  const options = talks.map((t, i) => `<option value="${i}" ${i === talkIdx ? 'selected' : ''}>${esc(t.partner)} · ${esc(t.status || (t.found ? 'open' : 'new'))} · ${t.data?.offers?.length ?? 0} offer${(t.data?.offers?.length ?? 0) === 1 ? '' : 's'}</option>`).join('')
  const d = cur.data
  const offers = d?.offers?.length
    ? d.offers.map(o => `<li class="sl-offer ${o.by === 'You' ? 'mine' : ''}"><div class="sl-offer__who"><b>${esc(o.by)}</b>${esc(o.when)}</div><div><div class="sl-sides">${tradeSide('You give', o.give)}${tradeSide('You get', o.get)}</div><p class="sl-eval"><span class="qk-badge ${o.verdict === 'send' ? 'qk-badge--ok' : o.verdict === 'decline' ? 'qk-badge--err' : 'qk-badge--warn'}">${esc(o.verdict)}</span> <b>${esc(o.delta)}</b> · ${esc(o.why)}</p></div></li>`).join('')
    : `<li class="sl-empty">No offers yet.</li>`
  const compose = d
    ? `<form class="sl-compose" id="compose" data-partner="${esc(cur.partner)}"><div><label for="give">You give</label><select id="give" class="sl-select sl-multi" multiple size="5">${(d.my_roster || []).map(tradeOption).join('')}</select></div><div><label for="get">You get from ${esc(cur.partner)}</label><select id="get" class="sl-select sl-multi" multiple size="5">${(d.partner_roster || []).map(tradeOption).join('')}</select></div><button class="qk-btn qk-btn--primary" data-run="trade" data-partner="${esc(cur.partner)}" type="button">Evaluate counter</button></form>`
    : ''
  return section('sec-trade', head + `<div class="sl-sec__body">
    <div class="sl-talkbar"><label for="talk-select" class="sl-keep" style="font-size:.75rem">Talk</label><select id="talk-select" class="sl-select">${options}</select><button class="qk-btn" data-run="trade">New talk</button></div>
    <ul class="sl-offers">${offers}</ul>
    ${compose}
    <div style="margin-top:1.25rem;padding-top:1rem;border-top:1px solid var(--qk-border)">${tradeFinder(suggestions)}</div>
    <p class="sl-src">One chat per talk; every offer or counter is a new turn, so the analyst keeps the whole negotiation in context.</p></div>`)
}

export function renderDigest(state) {
  const head = jobHead(state)
  if (!state.found) return section('sec-digest', head + emptyBody(state.job, state.title, state.what, state.running))
  const d = state.data
  const rows = d.games.map(g => `<tr class="${g.mine ? 'sl-mine' : ''}"><td>${esc(g.home)}</td><td class="num">${num(g.home_pts, 2)}</td><td class="num">${num(g.away_pts, 2)}</td><td>${esc(g.away)}</td><td class="num">${num(g.margin, 2)}</td><td>${esc(g.top_scorers || '')}</td></tr>`).join('')
  return section('sec-digest', head + `<div class="sl-sec__body">
    <p class="sl-summary">${esc(d.summary || '')}</p>
    <div class="qk-table-wrap"><table class="qk-table"><thead><tr><th>Home</th><th class="num">Pts</th><th class="num">Pts</th><th>Away</th><th class="num">Margin</th><th>Top scorers</th></tr></thead><tbody>${rows}</tbody></table></div>
    <p class="sl-src">${esc(d.source_note || '')}</p></div>`)
}

export function renderTrends(state) {
  const head = jobHead(state)
  if (!state.found) return section('sec-trends', head + emptyBody(state.job, state.title, state.what, state.running))
  const d = state.data
  const items = d.items.map(i => {
    const who = i.player ? `<span class="sl-p">${avatarHTML(i.player, true)}<span><b>${esc(i.player.name)}</b> ${esc(i.text)}</span></span>` : (i.link ? `${esc(i.text)} <a href="${esc(i.link)}" target="_blank" rel="noopener">source</a>` : esc(i.text))
    return `<li><time>${esc(i.time)}</time><div>${who}<span class="qk-chip src">${esc(i.source)}</span></div></li>`
  }).join('')
  return section('sec-trends', head + `<div class="sl-sec__body"><ul class="sl-timeline">${items}</ul>
    <p class="sl-src">${esc(d.source_note || '')}</p></div>`)
}

export function renderRetro(state) {
  const head = jobHead(state)
  if (!state.found) return section('sec-retro', head + emptyBody(state.job, state.title, state.what, state.running))
  const r = state.data
  const misses = (r.misses || []).map(m => `<tr><td>${esc(m.slot)}</td><td>${esc(m.started_name)}</td><td class="num">${num(m.started_pts, 2)}</td><td>${esc(m.better_name)}</td><td class="num">${num(m.better_pts, 2)}</td><td class="num">+${num(m.swing, 2)}</td></tr>`).join('')
  const swing = r.left >= Math.abs((r.opp ?? 0) - r.started) && !r.won
  return section('sec-retro', head + `<div class="sl-sec__body">
    <div class="sl-stats"><div class="sl-stat"><span>Started</span><b>${num(r.started, 2)}</b><small>${r.won ? 'won' : 'lost'}${r.opp != null ? ' to ' + num(r.opp, 2) : ''}</small></div><div class="sl-stat"><span>Best possible</span><b>${num(r.best, 2)}</b><small>from the players you owned</small></div><div class="sl-stat"><span>Left on the bench</span><b>${num(r.left, 1)}</b><small>${swing ? 'more than the margin: a winnable week' : 'less than the margin'}</small></div></div>
    <p class="sl-summary">${esc(r.summary || '')}</p>
    ${misses ? `<div class="qk-table-wrap"><table class="qk-table"><thead><tr><th>Slot</th><th>Started</th><th class="num">Pts</th><th>Better option</th><th class="num">Pts</th><th class="num">Swing</th></tr></thead><tbody>${misses}</tbody></table></div>` : ''}</div>`)
}

export function renderDraftBoard(state) {
  const head = jobHead(state)
  if (!state.found) return section('sec-draft', head + emptyBody(state.job, state.title, state.what, state.running))
  const d = state.data
  if (d.report_card) return renderDraftReportCard(state)
  const slots = Object.keys(d.slots).map(Number).sort((a, b) => a - b)
  const byRound = {}
  d.picks.forEach(p => { (byRound[p.round] = byRound[p.round] || {})[p.slot] = p })
  const cell = p => {
    if (!p) return '<td></td>'
    const adp = hasADP(p.adp) ? p.adp : null
    const delta = adp ? Math.round(p.pick_no - adp) : null
    const cls = delta == null ? '' : delta >= 8 ? 'fell' : delta <= -8 ? 'reach' : ''
    return `<td class="${p.mine ? 'me' : ''}"><span class="pos ${esc(p.player.pos || '')}">${esc(p.player.pos || '')}</span>${esc(p.player.name)}<span class="adp ${cls}">${adp ? (delta > 0 ? '+' : '') + delta + ' vs ADP ' + num(adp, 0) : ''}</span></td>`
  }
  const rows = Object.keys(byRound).sort((a, b) => a - b).map(r => `<tr><td class="num">${esc(r)}</td>${slots.map(s => cell(byRound[r][s])).join('')}</tr>`).join('')
  const mine = d.picks.filter(p => p.mine)
  const fell = mine.filter(p => hasADP(p.adp) && p.pick_no - p.adp >= 8).length
  const reach = mine.filter(p => hasADP(p.adp) && p.pick_no - p.adp <= -8).length
  return section('sec-draft', head + `<div class="sl-sec__body">
    <p class="sl-summary">Your column is highlighted. Green: the player fell 8 or more picks past ADP; red: a reach of 8 or more. ${fell} value picks, ${reach} reaches.</p>
    <div class="qk-table-wrap"><table class="qk-table sl-board"><thead><tr><th>Rd</th>${slots.map(s => `<th class="${s === d.my_slot ? 'me' : ''}">${s}. ${esc(d.slots[s])}</th>`).join('')}</tr></thead><tbody>${rows}</tbody></table></div>
    <p class="sl-src">ADP: Sleeper PPR ADP at draft time; players Sleeper had no ADP for show none. ${esc(d.type || '')} · ${d.rounds || ''} rounds.</p></div>`)
}

export function renderDraftReportCard(state) {
  const head = jobHead(state)
  if (!state.found) return section('sec-draft', head + emptyBody(state.job, state.title, state.what, state.running))
  const d = state.data
  const rows = (d.report_card || []).map(c => `<tr><td class="num">${c.round}</td><td class="num">${c.pick}</td><td><span class="sl-p">${avatarHTML(c.player, true)}<span>${esc(c.player.name)} <span class="sl-keep">${esc(c.player.pos || '')}</span></span></span></td><td class="num">${esc(c.drafted_as || '')}</td><td class="num">${esc(c.finished || '–')}</td><td class="num">${num(c.pts, 1)}</td><td><span class="qk-badge ${c.verdict === 'steal' ? 'qk-badge--ok' : c.verdict === 'reach' ? 'qk-badge--err' : ''}">${esc(c.verdict)}</span></td></tr>`).join('')
  const reaches = (d.report_card || []).filter(c => c.verdict === 'reach').length
  const steals = (d.report_card || []).filter(c => c.verdict === 'steal').length
  const allocText = d.pos_alloc ? Object.entries(d.pos_alloc).filter(([, n]) => n).map(([p, n]) => `${n} ${p}`).join(', ') : ''
  return section('sec-draft', head + `<div class="sl-sec__body">
    <p class="sl-summary">${steals} steals, ${reaches} reaches.${allocText ? ` Positional mix: ${esc(allocText)}.` : ''}</p>
    <div class="qk-table-wrap"><table class="qk-table"><thead><tr><th class="num">Rd</th><th class="num">Pick</th><th>Player</th><th class="num">Drafted as</th><th class="num">Finished</th><th class="num">Season pts</th><th>Verdict</th></tr></thead><tbody>${rows}</tbody></table></div>
    <p class="sl-src">Positional finish from Sleeper season stats (PPR). Reach = finished 8+ spots below where the position was taken; steal = 4+ above.</p></div>`)
}

export function renderDraftSide(state) {
  const head = jobHead(state)
  if (!state.found) return section('draft-side', head + emptyBody(state.job, state.title, state.what, state.running))
  const d = state.data
  if (!d.clock && !d.plan) return ''
  let html = ''
  if (d.clock) html = section('draft-clock', `<div class="sl-sec__head"><h2>On the clock</h2><div class="sl-sec__meta">${state.example ? '<span class="qk-badge">Example</span>' : ''}<span>${esc(state.agent || '')}</span></div></div><div class="sl-sec__body">
    <div class="sl-clock"><b>Pick ${d.clock.pick_no} (${esc(d.clock.pick_label || '')}) · ${d.clock.seconds_left}s left</b>${esc(d.clock.note)}</div>
    <p class="sl-src">Live drafts poll the picks feed every few seconds while you are within three picks of the clock.</p></div>`)
  if (d.plan) html += section('draft-plan', `<div class="sl-sec__head"><h2>Draft plan</h2><div class="sl-sec__meta">${state.example ? '<span class="qk-badge">Example</span>' : ''}<span>${esc(state.agent || '')} · pre-draft</span></div></div><div class="sl-sec__body"><div class="sl-tiers">${d.plan.map(p => `<div><b>${esc(p.title)}</b><span>${esc(p.note)}</span></div>`).join('')}</div></div>`)
  return html
}

export function renderReview(state) {
  const head = jobHead(state)
  if (!state.found) return section('sec-review', head + `<div class="sl-sec__body sl-empty"><span>Written after week 17. Until then, the week cards and hindsight carry the season.</span></div>`)
  const x = state.data
  const weeks = x.weeks || []
  const max = Math.max(1, ...weeks.map(w => w.best))
  const bars = weeks.map(w => `<div class="${w.won ? '' : 'loss'}" data-stop="${w.week}" title="Week ${w.week}: started ${num(w.started, 1)}, best ${num(w.best, 1)}${w.opp != null ? ', opponent ' + num(w.opp, 1) : ''}" style="height:${100 * w.best / max}%"><i style="height:${100 * w.started / w.best}%"></i></div>`).join('')
  const worst = weeks.length ? weeks.slice().sort((a, b) => (b.left ?? 0) - (a.left ?? 0))[0] : null
  return section('sec-review', head + `<div class="sl-sec__body">
    <div class="sl-stats"><div class="sl-stat"><span>Record</span><b>${x.wins}-${x.losses}</b><small>from draft slot ${x.draft_slot}</small></div><div class="sl-stat"><span>Points for</span><b>#${x.pf_rank}</b><small>${num(x.pf, 0)} · rank</small></div><div class="sl-stat"><span>Points against</span><b>#${x.pa_rank}</b><small>strength of schedule</small></div><div class="sl-stat"><span>Lineup efficiency</span><b>${num(x.eff, 1)}%</b><small>${num(x.left_total, 0)} pts left on the bench</small></div><div class="sl-stat"><span>Close losses</span><b>${x.close_losses}</b><small>by fewer than 10</small></div><div class="sl-stat"><span>Moves</span><b>${x.moves}</b><small>waivers and trades</small></div></div>
    <h3 style="font-size:.8125rem;margin:0 0 .25rem">Weekly: started (filled) vs best possible (bar); red is a loss.</h3>
    <div class="sl-bars">${bars}</div><div class="sl-bars-x">${weeks.map(w => `<span>${w.week}</span>`).join('')}</div>
    ${worst ? `<p class="sl-src">Worst week: ${worst.week}, ${num(worst.left, 1)} points left on the bench${worst.won ? '' : ' in a loss'}. Champion: ${esc(x.champion || '–')}.</p>` : ''}</div>`)
}

export function renderSeasonNotes(state) {
  const head = jobHead(state)
  if (!state.found) return section('side-notes', head + `<div class="sl-sec__body sl-empty"><span>No season notes yet.</span></div>`)
  const d = state.data
  return section('side-notes', head + `<div class="sl-sec__body"><ul class="sl-notes">${(d.notes || []).map(n => `<li>${esc(n)}</li>`).join('')}</ul></div>`)
}

export function renderSeasonAtGlance(x) {
  if (!x) return ''
  return section('side-season', `<div class="sl-sec__head"><h2>${esc(x.season)} at a glance</h2></div><div class="sl-sec__body"><dl class="sl-kv"><dt>Record</dt><dd>${x.wins}-${x.losses}</dd><dt>Points for</dt><dd>${num(x.pf, 0)} · #${x.pf_rank}</dd><dt>Points against</dt><dd>${num(x.pa, 0)} · #${x.pa_rank}</dd><dt>Lineup eff.</dt><dd>${num(x.eff, 1)}%</dd><dt>Champion</dt><dd>${esc(x.champion || '–')}</dd></dl></div>`)
}

export function renderCrossSeason(x) {
  const items = x?.cross_season_summary
  if (!items?.length) return ''
  return section('side-cross', `<div class="sl-sec__head"><h2>Recurring patterns</h2><div class="sl-sec__meta"><span class="qk-badge">Example</span></div></div><div class="sl-sec__body"><ul class="sl-mistakes">${items.map(i => `<li><b>${esc(i.n)}</b><p>${esc(i.text)}<br><small>${esc(i.detail || '')}</small></p></li>`).join('')}</ul></div>`)
}

export function renderStandingsSide(season) {
  const s = season.standings || []
  return section('side-standings', `<div class="sl-sec__head"><h2>Standings</h2><span class="sl-sec__meta">${season.playoff_line || 6} make the playoffs</span></div><div class="sl-sec__body"><ol class="sl-rows">${s.map((t, i) => `<li class="${i === (season.playoff_line || 6) - 1 ? 'line' : ''}"><span class="rank">${i + 1}</span><span class="main"><b>${esc(t.team)}${t.mine ? ' <span class="qk-badge">you</span>' : ''}</b><span>${esc(t.owner)}</span></span><span class="n"><b>${t.wins}-${t.losses}</b>${num(t.pf, 2)}</span></li>`).join('')}</ol></div>`)
}

export function renderMovesSide(season) {
  const m = season.recent_moves || []
  return section('side-moves', `<div class="sl-sec__head"><h2>Recent moves</h2></div><div class="sl-sec__body"><ul class="sl-rows">${m.map(t => `<li class="two"><span class="main"><b>${esc(t.label)}</b><span>${esc((t.by || []).join(' and '))}</span></span><span class="n">wk ${t.week}</span></li>`).join('')}</ul></div>`)
}

export function emptyState(job, title, agent, what) {
  return section(`sec-${job}`, jobHead({ title, agent, status: 'not_run' }) + emptyBody(job, title, what, false))
}

// artifactCounts: {stop: number of artifacts found}, for the rail's dots.
export function renderTimeline(currentStop, weekNow, isCurrentSeason, artifactCounts = {}) {
  const stops = ['draft', ...Array.from({ length: 17 }, (_, i) => String(i + 1)), 'review']
  const buttons = stops.map(s => {
    const isNow = isCurrentSeason && s === String(weekNow)
    const label = s === 'draft' ? 'Draft' : s === 'review' ? 'Review' : s
    const small = s === 'draft' || s === 'review' ? '' : isNow ? 'now' : Number(s) >= 15 ? 'playoff' : ''
    const dots = Math.min(artifactCounts[s] || 0, 5)
    return `<button class="sl-stop ${isNow ? 'now' : ''} ${s === 'draft' || s === 'review' ? 'wide' : ''}" aria-pressed="${s === currentStop}" data-stop="${esc(s)}"><small>${small}</small>${label}<span class="dots">${'<i></i>'.repeat(dots)}</span></button>`
  }).join('')
  return `<nav class="sl-rail" aria-label="Season timeline">${buttons}</nav>`
}

export function renderYears(seasons, currentSeason) {
  return `<nav class="sl-years" aria-label="Seasons">${seasons.map(s => `<button class="sl-year" data-season="${esc(s.season)}" aria-pressed="${s.season === currentSeason}">${esc(s.season)}<small>${s.wins}-${s.losses}</small></button>`).join('')}</nav>`
}

// items: [{job, label, agent, running, disabled}] - the kebab menu's list
// body, pulled out of main.js's renderMenu so it (and the whole menu) can
// be storied without live app state.
export function renderMenuList(heading, items) {
  const rows = items.map(i => `<li><button data-run="${esc(i.job)}" ${i.disabled || i.running ? 'disabled' : ''}>${esc(i.label)}<span>${esc(i.agent || '')}</span></button></li>`).join('')
  return `<li class="hd">${esc(heading)}</li>${rows}`
}

const KEBAB_SVG = '<svg width="20" height="20" viewBox="0 -960 960 960" fill="currentColor" aria-hidden="true"><path d="M480-160q-33 0-56.5-23.5T400-240q0-33 23.5-56.5T480-320q33 0 56.5 23.5T560-240q0 33-23.5 56.5T480-160Zm0-240q-33 0-56.5-23.5T400-480q0-33 23.5-56.5T480-560q33 0 56.5 23.5T560-480q0 33-23.5 56.5T480-400Zm0-240q-33 0-56.5-23.5T400-720q0-33 23.5-56.5T480-800q33 0 56.5 23.5T560-720q0 33-23.5 56.5T480-640Z"/></svg>'

// The full kebab menu, open, for stories - the served page's own
// index.html markup drives the real <details> (see main.js's renderMenu).
export function kebabMenuHTML(heading, items) {
  return `<details class="sl-menu" open><summary aria-label="Run a job" title="Run a job">${KEBAB_SVG}</summary><ul class="sl-menu__list">${renderMenuList(heading, items)}</ul></details>`
}
