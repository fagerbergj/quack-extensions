import { page, mobileFrame } from './helpers.js'
import { renderLineup } from '../../static/render.js'
import lineup from '../../fixtures/lineup.json'

const envelope = { job: 'lineup', title: 'Start / sit', agent: 'lineup-analyst', found: true, example: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data: lineup }

export default { title: 'Cards/StartSit' }

export const Default = { render: () => page(renderLineup(envelope)) }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderLineup(envelope)) }

// Backward compatibility: an artifact written before floor/ceiling, replaces,
// plan and watch existed - the card must still render (no h2h ranges/win
// chance/IN badges/plan chip/watch strip, since that data is simply absent).
const stripNew = row => { const { floor, ceiling, replaces, ...rest } = row; return rest }
const legacyData = {
  ...lineup,
  plan: undefined,
  watch: undefined,
  starters: lineup.starters.map(stripNew),
  bench: (lineup.bench || []).map(stripNew),
  opponent_starters: (lineup.opponent_starters || []).map(stripNew),
}
const legacyEnvelope = { ...envelope, data: legacyData }
export const Legacy = { render: () => page(renderLineup(legacyEnvelope)) }
