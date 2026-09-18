import { sidebar, mobileFrame } from './helpers.js'
import { renderSeasonNotes } from '../../static/render.js'
import notes from '../../fixtures/season-notes.json'

const envelope = { title: 'Season notes', agent: 'trend-scout', found: true, example: true, status: 'done', chatHref: '/chat/ext:sleeper:demo', data: notes }

export default { title: 'Cards/SeasonNotes' }

export const Default = { render: () => sidebar(renderSeasonNotes(envelope)) }
export const Dark = { ...Default, globals: { theme: 'dark' } }
export const MobileViewport390 = { render: () => mobileFrame(renderSeasonNotes(envelope)) }
