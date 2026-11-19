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

    // Crop editor. The frame is kept in fractions of the page as rotated
    // (0..1) and posted in pixels of it. The rotation is quarter turns plus
    // a fine angle, clockwise, as the server applies them.
    cropOpen: false,
    cropPending: false,
    gridOn: false,
    page: { w: 0, h: 0 },
    rot: { turns: 0, angle: 0 },
    shown: { turns: 0, angle: 0 }, // rotation of the loaded image
    loading: null, // rotation of the image being fetched
    rotTimer: null,
    frame: { x: 0, y: 0, w: 1, h: 1 },

    get angleLabel() {
      const a = this.rot.angle
      return (a > 0 ? '+' : a < 0 ? '−' : '') + Math.abs(a).toFixed(1) + '°'
    },

    openCrop(e) {
      const d = e.currentTarget.dataset
      this.page = { w: Number(d.w), h: Number(d.h), src: d.src }
      this.rot = { turns: Number(d.turns) || 0, angle: Number(d.angle) || 0 }
      const v = this.viewSize()
      const c = (d.crop || '').split(',').map(Number)
      this.frame = c.length === 4 ? { x: c[0] / v.w, y: c[1] / v.h, w: c[2] / v.w, h: c[3] / v.h } : { x: 0, y: 0, w: 1, h: 1 }
      this.$refs.cropFile.value = d.file
      this.$refs.cropAction.value = 'save'
      this.gridOn = false
      this.shown = { ...this.rot }
      this.loadRot()
      this.tiltPreview()
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
      clearTimeout(this.rotTimer)
      this.setInert(false)
      const file = this.$refs.cropFile.value
      this.$nextTick(() => {
        const btn = [...document.querySelectorAll('.sheet-crop')].find((b) => b.dataset.file === file)
        if (btn) btn.focus()
      })
    },

    // Size of the page once rotated, as crop.Rotation.Size computes it.
    viewSize(rot = this.rot) {
      let { w, h } = this.page
      if (rot.turns % 2) [w, h] = [h, w]
      if (Math.abs(rot.angle) < 0.01) return { w, h }
      const a = Math.abs(rot.angle) * Math.PI / 180
      const s = Math.sin(a), c = Math.cos(a)
      return { w: Math.round(w * c + h * s), h: Math.round(w * s + h * c) }
    },
    // The largest frame holding none of the white corners a fine turn
    // uncovers, in fractions of the turned page (as crop.Inscribed).
    inscribed(rot) {
      let { w, h } = this.page
      if (rot.turns % 2) [w, h] = [h, w]
      if (Math.abs(rot.angle) < 0.01) return { x: 0, y: 0, w: 1, h: 1 }
      const v = this.viewSize(rot)
      const a = Math.abs(rot.angle) * Math.PI / 180
      const s = Math.sin(a), c = Math.cos(a)
      const long = Math.max(w, h), short = Math.min(w, h)
      let iw, ih
      if (short <= 2 * s * c * long) {
        const x = short / 2
        ;[iw, ih] = w >= h ? [x / s, x / c] : [x / c, x / s]
      } else {
        const c2 = c * c - s * s
        ;[iw, ih] = [(w * c - h * s) / c2, (h * c - w * s) / c2]
      }
      iw = Math.min(v.w, Math.floor(iw) - 2)
      ih = Math.min(v.h, Math.floor(ih) - 2)
      return { x: (v.w - iw) / 2 / v.w, y: (v.h - ih) / 2 / v.h, w: iw / v.w, h: ih / v.h }
    },
    turnBy(e) {
      this.setRot({ turns: (this.rot.turns + Number(e.currentTarget.dataset.by) + 4) % 4, angle: this.rot.angle })
    },
    tiltBy(e) { this.setRot({ ...this.rot, angle: this.rot.angle + Number(e.currentTarget.dataset.by) }) },
    tiltTo(e) { this.setRot({ ...this.rot, angle: Number(e.currentTarget.value) }) },
    level() { this.setRot({ ...this.rot, angle: 0 }) },
    // The frame turns with the page, so a crop survives a tilt. A frame
    // around the whole page becomes the largest one without white
    // corners. The loaded image turns at once (CSS) and the server's
    // rendering follows when the angle settles; meanwhile the frame hides
    // and can't be dragged, as it no longer matches the picture.
    setRot(r) {
      r.angle = Math.round(Math.min(45, Math.max(-45, r.angle)) * 10) / 10
      const old = this.rot, f = this.frame
      const ov = this.viewSize(old), nv = this.viewSize(r)
      const whole = f.x <= 0.001 && f.y <= 0.001 && f.w >= 0.999 && f.h >= 0.999
      const wasInscribed = ['x', 'y', 'w', 'h'].every((k) => Math.abs(f[k] - this.inscribed(old)[k]) < 0.002)
      if (whole || wasInscribed) {
        this.frame = this.inscribed(r)
      } else {
        const dTurns = (r.turns - old.turns + 4) % 4
        const d = (dTurns * 90 + r.angle - old.angle) * Math.PI / 180
        const cx = (f.x + f.w / 2) * ov.w - ov.w / 2, cy = (f.y + f.h / 2) * ov.h - ov.h / 2
        const nx = cx * Math.cos(d) - cy * Math.sin(d), ny = cx * Math.sin(d) + cy * Math.cos(d)
        let fw = f.w * ov.w, fh = f.h * ov.h
        if (dTurns % 2) [fw, fh] = [fh, fw]
        fw = Math.min(fw, nv.w); fh = Math.min(fh, nv.h)
        const x = Math.min(nv.w - fw, Math.max(0, nx + nv.w / 2 - fw / 2))
        const y = Math.min(nv.h - fh, Math.max(0, ny + nv.h / 2 - fh / 2))
        this.frame = { x: x / nv.w, y: y / nv.h, w: fw / nv.w, h: fh / nv.h }
      }
      this.rot = r
      this.gridOn = true
      this.drawCrop()
      clearTimeout(this.rotTimer)
      if (r.turns !== this.shown.turns) {
        this.loadRot()
      } else {
        this.rotTimer = setTimeout(() => this.loadRot(), 350)
      }
      this.tiltPreview()
    },
    // Until the matching rendering is in, the picture on screen (perhaps
    // another page's) doesn't match the frame.
    get previewStale() {
      return this.loading !== null || this.rot.turns !== this.shown.turns || this.rot.angle !== this.shown.angle
    },
    loadRot() {
      this.loading = { ...this.rot }
      const { turns, angle } = this.loading
      this.$refs.cropImg.src = this.page.src + (turns || angle ? `&turns=${turns}&angle=${angle}` : '')
    },
    cropLoaded() {
      if (this.loading) this.shown = this.loading
      this.loading = null
      this.tiltPreview()
    },
    cropFailed() {
      this.loading = null
      this.tiltPreview()
    },
    tiltPreview() {
      const d = this.rot.turns === this.shown.turns ? this.rot.angle - this.shown.angle : 0
      this.$refs.cropImg.style.transform = d ? `rotate(${d}deg)` : ''
      const hide = this.previewStale ? 'hidden' : ''
      for (const el of [this.$refs.cropBox, this.$refs.cropHole]) el.style.visibility = hide
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
      if (e.button !== 0 || this.previewStale) return
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
      const { w, h } = this.viewSize()
      this.$refs.cropTurns.value = this.rot.turns
      this.$refs.cropAngle.value = this.rot.angle
      this.$refs.cropRange.value = this.rot.angle
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
