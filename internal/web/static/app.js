// scan-hub client state. Alpine runs as its CSP build: components are
// registered here and templates reference only their properties and methods.
document.addEventListener('alpine:init', () => {
  // Tap-twice confirmation: native confirm() is unreliable in embedded and
  // mobile browsers. The first tap arms the real button for 4 s.
  // While armed it carries data-armed, so polling doesn't replace it (see
  // below).
  Alpine.data('confirm', () => ({
    armed: false,
    timer: null,
    get idle() { return !this.armed },
    arm() {
      this.armed = true
      this.$root.dataset.armed = ''
      clearTimeout(this.timer)
      this.timer = setTimeout(() => {
        this.armed = false
        delete this.$root.dataset.armed
      }, 4000)
    },
  }))

  Alpine.data('dismiss', () => ({
    close() { this.$root.remove() },
  }))

  // The session page shows one scan form per scanner; `scanner` is the id
  // of the visible one.
  Alpine.data('sessionPage', () => ({
    scanner: '',
    lightbox: false,
    lightboxSrc: '',

    init() { this.scanner = this.$root.dataset.scanner || '' },

    openLightbox(e) {
      this.lightboxSrc = e.currentTarget.dataset.full
      this.lightbox = true
    },
    closeLightbox() {
      this.lightbox = false
      this.lightboxSrc = ''
    },
  }))

  // A switch button or scan form of one scanner (data-scanner on its root),
  // nested in sessionPage.
  Alpine.data('scannerPane', () => ({
    get shown() { return this.scanner === this.$root.dataset.scanner },
    get pressed() { return this.shown ? 'true' : 'false' },
    pick() { this.scanner = this.$root.dataset.scanner },
  }))
})

// A polling region (e.g. the device cards) skips its refresh while it holds
// an armed confirmation, which the swap would otherwise reset between taps.
// The armed button's own request still goes through.
document.addEventListener('htmx:before:request', (e) => {
  const el = e.target
  if (!el.getAttribute || !/\bevery\b/.test(el.getAttribute('hx-trigger') || '')) return
  const armed = el.querySelector('[data-armed]')
  if (armed && !armed.contains(el)) e.preventDefault()
})
