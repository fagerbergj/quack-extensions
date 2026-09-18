// Shared story wrappers: every story returns real DOM (html-vite accepts a
// string too, but an element lets render-check measure scrollWidth).
export function page(html, maxWidth = '48rem') {
  const el = document.createElement('div')
  el.className = 'qk-page'
  el.style.maxWidth = maxWidth
  el.innerHTML = html
  return el
}

// The MobileViewport390 story variant: a fixed-width frame so the layout
// previews at phone width even when Storybook's own canvas is wider.
// render-check additionally resizes the real viewport for every story.
export function mobileFrame(html) {
  const outer = document.createElement('div')
  outer.className = 'qk-page'
  outer.style.cssText = 'width:390px;max-width:390px;border:1px solid var(--qk-border);overflow-x:hidden'
  outer.innerHTML = html
  return outer
}

export function sidebar(html) {
  const el = document.createElement('div')
  el.className = 'qk-page'
  el.style.maxWidth = '20rem'
  el.innerHTML = html
  return el
}
