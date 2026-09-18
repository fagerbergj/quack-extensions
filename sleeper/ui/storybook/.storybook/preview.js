// kit.css is vendored from quack/frontend/public/assets/ext/v1/kit.css - it's
// a separate repo, so there's no path to import the real file; the v1
// contract is frozen-additive, so drift risk is low. Re-copy it here if v1
// ever changes. page.css is imported from its real served location, so it
// never drifts from what the extension actually ships.
import './kit.css'
import '../../static/page.css'
import { installAvatarFallback } from '../../static/render.js'

installAvatarFallback()

export const globalTypes = {
  theme: {
    description: 'Theme',
    defaultValue: 'light',
    toolbar: { icon: 'circlehollow', items: ['light', 'dark'], dynamicTitle: true },
  },
}

// Mirrors quack/frontend/.storybook/preview.tsx's withTheme decorator, but
// stamps data-theme (this page has no Tailwind `dark:` class toggle) - see
// page.css's :root[data-theme] blocks, which kit.css v1 itself doesn't define.
// Each story's own render wraps its markup in .qk-page itself (helpers.js's
// page()), so this decorator only needs to set the theme and pass through -
// wrapping again here would fight a story-level width decorator (mobile
// frame) over which element ends up outermost.
const withTheme = (storyFn, context) => {
  document.documentElement.dataset.theme = context.globals.theme || 'light'
  return storyFn()
}

export const decorators = [withTheme]

export default {
  parameters: { layout: 'fullscreen' },
}
