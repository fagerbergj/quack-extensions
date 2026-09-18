import { page, mobileFrame } from './helpers.js'
import { renderTrade } from '../../static/render.js'
import trade from '../../fixtures/trade.json'

const envelope = { job: 'trade', title: 'Trade talks', agent: 'trade-analyst', found: true, status: 'done', chatHref: '/chat/ext:sleeper:demo' }
const talks = [
  { partner: trade.partner, found: true, example: true, status: trade.status, data: trade },
  { partner: 'Jockstrappers', found: false, status: 'declined', data: null },
]

export default { title: 'Cards/TradeTalks' }

export const Default = { render: () => page(renderTrade(envelope, talks, 0), '52rem') }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderTrade(envelope, talks, 0)) }
