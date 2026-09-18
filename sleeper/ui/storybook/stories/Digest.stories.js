import { page, mobileFrame } from './helpers.js'
import { renderDigest } from '../../static/render.js'
import digest from '../../fixtures/digest.json'

const envelope = { job: 'digest', title: 'Week 2 preview', agent: 'league-reporter', found: true, example: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data: digest }

export default { title: 'Cards/Digest' }

export const Default = { render: () => page(renderDigest(envelope), '52rem') }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderDigest(envelope)) }
