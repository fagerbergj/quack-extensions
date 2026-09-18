import { page, mobileFrame } from './helpers.js'
import { renderTrade } from '../../static/render.js'
import trade from '../../fixtures/trade.json'

const envelope = { job: 'trade', title: 'Trade talks', agent: 'trade-analyst', found: true, status: 'done', chatHref: '/chat/ext:sleeper:demo' }
const talks = [
  { partner: trade.partner, partner_id: trade.partner_id, found: true, example: true, status: trade.status, data: trade },
  { partner: 'Jockstrappers', partner_id: '859928030787866624', found: false, status: 'declined', data: null },
]
const partners = [
  { id: trade.partner_id, name: 'Rice Cooker' },
  { id: '859928030787866624', name: 'Jockstrappers' },
  { id: '860265672989609984', name: 'Pitts and Giggles' },
  { id: '860209887672635392', name: 'Achane Magic' },
]

export default { title: 'Cards/TradeTalks' }

export const Default = { render: () => page(renderTrade(envelope, talks, 0, partners), '52rem') }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderTrade(envelope, talks, 0, partners)) }
export const NoTalksYet = { render: () => page(renderTrade({ ...envelope, found: false }, [], 0, partners), '52rem') }
