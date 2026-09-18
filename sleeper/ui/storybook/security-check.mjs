// Renders every card with a hostile payload in place of every numeric-typed
// artifact field (fixture values only - Host.ReadArtifact bytes are never
// schema-validated on the serve path, so a string in a number field is a
// real, reachable case) and asserts the payload never survives unescaped
// into the HTML output. Node-only, no browser: render.js is pure string-in/
// string-out, so this needs no DOM.
import { readFile } from 'node:fs/promises'
import path from 'node:path'
import * as R from '../static/render.js'

const FIXTURES_DIR = path.join(import.meta.dirname, '..', 'fixtures')
const PAYLOAD = '<img src=x onerror=alert(1)>'

async function loadFixture(name) {
  return JSON.parse(await readFile(path.join(FIXTURES_DIR, name + '.json'), 'utf8'))
}

// Deep-clones data, replacing every number leaf (at any depth, in objects
// and arrays alike) with the hostile payload string.
function poisonNumbers(data) {
  if (typeof data === 'number') return PAYLOAD
  if (Array.isArray(data)) return data.map(poisonNumbers)
  if (data && typeof data === 'object') {
    const out = {}
    for (const [k, v] of Object.entries(data)) out[k] = poisonNumbers(v)
    return out
  }
  return data
}

const doneEnvelope = data => ({ job: 'x', title: 'x', agent: 'x', found: true, example: false, status: 'done', chatHref: '/chat/x', data })
const invalidEnvelope = text => ({ job: 'x', title: 'x', agent: 'x', found: true, invalid: true, example: false, status: 'done', text })

async function checks() {
  const out = []

  // Invalid-artifact state: raw agent text lands in a <pre> via esc(), the
  // only escaping path for state.text - covered once via renderLineup (the
  // plain job-card shape) and once via renderTrade (the per-talk shape).
  out.push(['renderLineup (invalid)', R.renderLineup(invalidEnvelope(PAYLOAD))])
  out.push(['renderTrade (invalid)', R.renderTrade(doneEnvelope(null), [{ partner: 'x', partner_id: '1', found: true, example: false, invalid: true, text: PAYLOAD }], 0, [{ id: '1', name: 'Team' }])])

  const lineup = poisonNumbers(await loadFixture('lineup'))
  out.push(['renderLineup', R.renderLineup(doneEnvelope(lineup))])

  const waivers = poisonNumbers(await loadFixture('waivers'))
  out.push(['renderWaivers', R.renderWaivers(doneEnvelope(waivers))])

  const trade = poisonNumbers(await loadFixture('trade'))
  const talks = [{ partner: trade.partner, partner_id: String(trade.partner_id), found: true, example: false, status: 'open', data: trade }]
  const partners = [{ id: '1', name: 'Team' }]
  out.push(['renderTrade', R.renderTrade(doneEnvelope(null), talks, 0, partners)])

  const digest = poisonNumbers(await loadFixture('digest'))
  out.push(['renderDigest', R.renderDigest(doneEnvelope(digest))])

  const trends = poisonNumbers(await loadFixture('trends'))
  out.push(['renderTrends', R.renderTrends(doneEnvelope(trends))])

  const retro = poisonNumbers(await loadFixture('retro'))
  out.push(['renderRetro', R.renderRetro(doneEnvelope(retro))])

  const draft = poisonNumbers(await loadFixture('draft'))
  out.push(['renderDraftBoard', R.renderDraftBoard(doneEnvelope(draft))])
  out.push(['renderDraftSide', R.renderDraftSide(doneEnvelope(draft))])

  const history = poisonNumbers(await loadFixture('history'))
  const draftReportCard = { ...draft, report_card: history.report_card, pos_alloc: history.pos_alloc }
  out.push(['renderDraftReportCard', R.renderDraftBoard(doneEnvelope(draftReportCard))])
  out.push(['renderReview', R.renderReview(doneEnvelope(history))])
  out.push(['renderSeasonAtGlance', R.renderSeasonAtGlance(history)])
  out.push(['renderCrossSeason', R.renderCrossSeason(history)])

  const notes = poisonNumbers(await loadFixture('season-notes'))
  out.push(['renderSeasonNotes', R.renderSeasonNotes(doneEnvelope(notes))])

  const season = poisonNumbers(await loadFixture('season'))
  out.push(['renderStandingsSide', R.renderStandingsSide(season)])
  out.push(['renderMovesSide', R.renderMovesSide(season)])

  const seasons = poisonNumbers(await loadFixture('seasons'))
  out.push(['renderYears', R.renderYears(seasons.seasons, seasons.current_season)])
  out.push(['renderTimeline', R.renderTimeline('2', 2, true, { 2: 3 })])

  return out
}

async function main() {
  const results = await checks()
  let failed = 0
  for (const [name, html] of results) {
    if (html.includes(PAYLOAD)) {
      failed++
      console.error(`FAIL ${name}: hostile payload survived unescaped into the output`)
    }
    if (/<img\s[^>]*onerror=/i.test(html)) {
      failed++
      console.error(`FAIL ${name}: an unescaped <img onerror=...> tag reached the output`)
    }
  }
  console.log(`security-check: ${results.length} renders checked, ${failed} failure(s)`)
  if (failed > 0) process.exit(1)
}

main().catch(err => { console.error(err); process.exit(1) })
