import { page, mobileFrame } from './helpers.js'
import { renderWaivers } from '../../static/render.js'
import waivers from '../../fixtures/waivers.json'

const envelope = { job: 'waivers', title: 'Waivers', agent: 'waiver-scout', found: true, example: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data: waivers }

export default { title: 'Cards/Waivers' }

export const Default = { render: () => page(renderWaivers(envelope)) }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderWaivers(envelope)) }

// Backward compatibility: an artifact written before waiver_type/
// my_priority/teams/also_checked existed - no sub line, no "how claims run"
// fold, no "also checked" fold, but the numbered claim rows still render.
const { waiver_type, my_priority, teams, also_checked, ...legacyData } = waivers
const legacyEnvelope = { ...envelope, data: legacyData }
export const Legacy = { render: () => page(renderWaivers(legacyEnvelope)) }
