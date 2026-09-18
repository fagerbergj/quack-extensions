import { page, mobileFrame } from './helpers.js'
import { renderLineup } from '../../static/render.js'
import lineup from '../../fixtures/lineup.json'

const envelope = { job: 'lineup', title: 'Start / sit', agent: 'lineup-analyst', found: true, example: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data: lineup }

export default { title: 'Cards/StartSit' }

export const Default = { render: () => page(renderLineup(envelope)) }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderLineup(envelope)) }
