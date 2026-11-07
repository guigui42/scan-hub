// scan-hub client state. Alpine runs as its CSP build: components are
// registered here and templates reference only their properties and methods.
document.addEventListener('alpine:init', () => {
  // Tap-twice confirmation: native confirm() is unreliable in embedded and
  // mobile browsers. The first tap arms the real button for 4 s.
  Alpine.data('confirm', () => ({
    armed: false,
    timer: null,
    get idle() { return !this.armed },
    arm() {
      this.armed = true
      clearTimeout(this.timer)
      this.timer = setTimeout(() => { this.armed = false }, 4000)
    },
  }))

  Alpine.data('dismiss', () => ({
    close() { this.$root.remove() },
  }))

  Alpine.data('sessionPage', () => ({
    scanner: 'scansnap',
    lightbox: false,
    lightboxSrc: '',

    init() { this.scanner = this.$root.dataset.scanner || 'scansnap' },

    get isScanSnap() { return this.scanner === 'scansnap' },
    get isEpson() { return this.scanner === 'epson' },
    get ssPressed() { return this.isScanSnap ? 'true' : 'false' },
    get epPressed() { return this.isEpson ? 'true' : 'false' },
    useScanSnap() { this.scanner = 'scansnap' },
    useEpson() { this.scanner = 'epson' },

    openLightbox(e) {
      this.lightboxSrc = e.currentTarget.dataset.full
      this.lightbox = true
    },
    closeLightbox() {
      this.lightbox = false
      this.lightboxSrc = ''
    },
  }))
})
