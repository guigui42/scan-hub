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

    // Crop editor. The frame is kept in fractions of the page (0..1) and
    // posted in pixels of the original image.
    cropOpen: false,
    cropPending: false,
    page: { w: 0, h: 0 },
    frame: { x: 0, y: 0, w: 1, h: 1 },

    openCrop(e) {
      const d = e.currentTarget.dataset
      const w = Number(d.w), h = Number(d.h)
      const c = (d.crop || '').split(',').map(Number)
      this.page = { w, h }
      this.frame = c.length === 4 ? { x: c[0] / w, y: c[1] / h, w: c[2] / w, h: c[3] / h } : { x: 0, y: 0, w: 1, h: 1 }
      this.$refs.cropFile.value = d.file
      this.$refs.cropAction.value = 'save'
      this.$refs.cropImg.src = d.src
      this.cropOpen = true
      this.cropPending = false
      this.drawCrop()
      this.setInert(true)
      this.$nextTick(() => this.$refs.cropSave.focus())
    },
    // The editor stays open until its request finishes, so the old crop
    // can't be downloaded or sent meanwhile.
    cropBusy() { this.cropPending = true },
    closeCrop(e) {
      if (!this.cropOpen) return
      if (this.cropPending && e && e.type !== 'htmx:finally:request') return
      this.cropOpen = false
      this.cropPending = false
      this.setInert(false)
      const file = this.$refs.cropFile.value
      this.$nextTick(() => {
        const btn = [...document.querySelectorAll('.sheet-crop')].find((b) => b.dataset.file === file)
        if (btn) btn.focus()
      })
    },
    // A modal dialog: everything outside it is inert while it's open.
    setInert(on) {
      for (let el = this.$refs.cropper; el && el !== document.body; el = el.parentElement) {
        for (const sib of el.parentElement.children) {
          if (sib !== el) sib.inert = on
        }
      }
    },
    setCropAction(e) { this.$refs.cropAction.value = e.currentTarget.dataset.action },

    cropDown(e) {
      if (e.button !== 0) return
      e.preventDefault()
      const stage = e.currentTarget
      const at = (ev) => {
        const r = this.$refs.cropImg.getBoundingClientRect()
        return {
          x: Math.min(1, Math.max(0, (ev.clientX - r.left) / r.width)),
          y: Math.min(1, Math.max(0, (ev.clientY - r.top) / r.height)),
        }
      }
      const start = at(e), orig = { ...this.frame }
      const mode = e.target.dataset.mode || 'new'
      // Corner drags and new frames span from a fixed point to the pointer.
      const fixed = {
        new: start,
        nw: { x: orig.x + orig.w, y: orig.y + orig.h },
        ne: { x: orig.x, y: orig.y + orig.h },
        sw: { x: orig.x + orig.w, y: orig.y },
        se: { x: orig.x, y: orig.y },
      }[mode]
      const move = (ev) => {
        const p = at(ev)
        if (mode === 'move') {
          this.frame = {
            ...orig,
            x: Math.min(1 - orig.w, Math.max(0, orig.x + p.x - start.x)),
            y: Math.min(1 - orig.h, Math.max(0, orig.y + p.y - start.y)),
          }
        } else {
          this.frame = {
            x: Math.min(fixed.x, p.x), y: Math.min(fixed.y, p.y),
            w: Math.abs(p.x - fixed.x), h: Math.abs(p.y - fixed.y),
          }
        }
        this.drawCrop()
      }
      const up = () => {
        stage.removeEventListener('pointermove', move)
        stage.removeEventListener('pointerup', up)
        stage.removeEventListener('pointercancel', up)
        // A click or a tiny frame keeps the previous one.
        if (this.frame.w < 0.02 || this.frame.h < 0.02) {
          this.frame = orig
          this.drawCrop()
        }
      }
      stage.setPointerCapture(e.pointerId)
      stage.addEventListener('pointermove', move)
      stage.addEventListener('pointerup', up)
      stage.addEventListener('pointercancel', up)
    },

    // Styles are set through the CSSOM: the CSP forbids style attributes.
    drawCrop() {
      const f = this.frame
      for (const el of [this.$refs.cropBox, this.$refs.cropHole]) {
        el.style.left = f.x * 100 + '%'
        el.style.top = f.y * 100 + '%'
        el.style.width = f.w * 100 + '%'
        el.style.height = f.h * 100 + '%'
      }
      const { w, h } = this.page
      const x = Math.round(f.x * w), y = Math.round(f.y * h)
      this.$refs.cropX.value = x
      this.$refs.cropY.value = y
      this.$refs.cropW.value = Math.min(w - x, Math.round(f.w * w))
      this.$refs.cropH.value = Math.min(h - y, Math.round(f.h * h))
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
