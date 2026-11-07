// Package session stores scan sessions on disk: ordered pages, thumbnails
// and a manifest, each written atomically (temp file, fsync, rename).
package session

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"syscall"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff" // DecodeConfig/Decode for B&W pages

	"github.com/guigui42/scan-hub/internal/fsutil"
)

var (
	// ErrNotFound is returned for an unknown session or page.
	ErrNotFound = errors.New("not found")
	// ErrLimit is returned when a page would exceed a configured limit.
	ErrLimit = errors.New("limit reached")
	// ErrChanged is returned when a session changed since it was read.
	ErrChanged = errors.New("session changed")
)

var idPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

const manifestName = "manifest.json"

// Page describes one stored page.
type Page struct {
	File    string    `json:"file"`
	Thumb   string    `json:"thumb,omitempty"`
	MIME    string    `json:"mime"`
	SHA256  string    `json:"sha256"`
	Size    int64     `json:"size"`
	Width   int       `json:"width,omitempty"`
	Height  int       `json:"height,omitempty"`
	DPI     int       `json:"dpi,omitempty"`
	Scanner string    `json:"scanner"`
	Sheet   int       `json:"sheet"`
	Side    int       `json:"side"`
	Added   time.Time `json:"added"`
}

// Manifest is the persisted state of a session.
type Manifest struct {
	ID      string    `json:"id"`
	Source  string    `json:"source"`
	Title   string    `json:"title,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Pages   []Page    `json:"pages"`
}

// Bytes is the total size of the session's pages.
func (m Manifest) Bytes() int64 {
	var n int64
	for _, p := range m.Pages {
		n += p.Size
	}
	return n
}

// Options are store limits. Zero values take the defaults.
type Options struct {
	MaxPages        int   // per session, default 200
	MaxSessionBytes int64 // default 1 GiB
	MinFreeBytes    int64 // refuse new pages below this, default 512 MiB; negative disables
	ThumbWidth      int   // default 360 px
}

// Store holds sessions under one root directory.
type Store struct {
	root string
	opt  Options
	mu   sync.Mutex
	free func(string) (int64, error)
}

// Open creates root if needed.
func Open(root string, opt Options) (*Store, error) {
	if opt.MaxPages == 0 {
		opt.MaxPages = 200
	}
	if opt.MaxSessionBytes == 0 {
		opt.MaxSessionBytes = 1 << 30
	}
	if opt.MinFreeBytes == 0 {
		opt.MinFreeBytes = 512 << 20
	}
	if opt.ThumbWidth == 0 {
		opt.ThumbWidth = 360
	}
	if err := fsutil.MkdirAllDurable(root, 0o700); err != nil {
		return nil, err
	}
	return &Store{root: root, opt: opt, free: freeBytes}, nil
}

// Create starts an empty session.
func (s *Store) Create(source string) (Manifest, error) {
	var rnd [3]byte
	rand.Read(rnd[:])
	now := time.Now().UTC()
	m := Manifest{
		ID:      now.Format("20060102-150405") + "-" + hex.EncodeToString(rnd[:]),
		Source:  source,
		Created: now,
		Updated: now,
		Pages:   []Page{},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Mkdir(s.dir(m.ID), 0o700); err != nil {
		return Manifest{}, err
	}
	if err := fsutil.SyncDir(s.root); err != nil {
		return Manifest{}, err
	}
	return m, s.writeManifest(m)
}

// Get loads a session manifest.
func (s *Store) Get(id string) (Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(id)
}

// List returns all sessions, most recently updated first. Unreadable
// sessions are skipped.
func (s *Store) List() ([]Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ents, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if m, err := s.load(e.Name()); err == nil {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b Manifest) int { return b.Updated.Compare(a.Updated) })
	return out, nil
}

// SetTitle sets the optional document title.
func (s *Store) SetTitle(id, title string) (Manifest, error) {
	if len(title) > 200 {
		return Manifest{}, fmt.Errorf("%w: title longer than 200 bytes", ErrLimit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load(id)
	if err != nil {
		return m, err
	}
	m.Title = title
	m.Updated = time.Now().UTC()
	return m, s.writeManifest(m)
}

// AddPage stores data as the next page of session id.
func (s *Store) AddPage(id string, data []byte, scanner string, sheet, side int) (Page, error) {
	mime, ext := sniff(data)
	if mime == "" {
		return Page{}, errors.New("unsupported page format")
	}
	sum := sha256.Sum256(data)
	p := Page{
		MIME:    mime,
		SHA256:  hex.EncodeToString(sum[:]),
		Size:    int64(len(data)),
		Scanner: scanner,
		Sheet:   sheet,
		Side:    side,
		Added:   time.Now().UTC(),
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		p.Width, p.Height = cfg.Width, cfg.Height
	}
	switch mime {
	case "image/jpeg":
		p.DPI = jfifDPI(data)
	case "image/tiff":
		p.DPI = tiffDPI(data)
	}
	thumb := s.thumbnail(data)

	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load(id)
	if err != nil {
		return Page{}, err
	}
	switch {
	case len(m.Pages) >= s.opt.MaxPages:
		return Page{}, fmt.Errorf("%w: %d pages per session", ErrLimit, s.opt.MaxPages)
	case m.Bytes()+p.Size > s.opt.MaxSessionBytes:
		return Page{}, fmt.Errorf("%w: %d bytes per session", ErrLimit, s.opt.MaxSessionBytes)
	}
	if s.opt.MinFreeBytes >= 0 {
		if free, err := s.free(s.root); err == nil && free-p.Size < s.opt.MinFreeBytes {
			return Page{}, fmt.Errorf("%w: less than %d bytes free on disk", ErrLimit, s.opt.MinFreeBytes)
		}
	}

	base := fmt.Sprintf("page-%s-%s", p.Added.Format("150405.000"), p.SHA256[:8])
	p.File = base + ext
	dir := s.dir(id)
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, p.File), data, 0o600); err != nil {
		return Page{}, err
	}
	if thumb != nil {
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, base+".thumb.jpg"), thumb, 0o600); err == nil {
			p.Thumb = base + ".thumb.jpg"
		}
	}
	m.Pages = append(m.Pages, p)
	m.Updated = p.Added
	if err := s.writeManifest(m); err != nil {
		s.removeFiles(dir, p)
		return Page{}, err
	}
	return p, nil
}

// DeletePage removes the page stored as file from session id.
func (s *Store) DeletePage(id, file string) (Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load(id)
	if err != nil {
		return m, err
	}
	i := slices.IndexFunc(m.Pages, func(p Page) bool { return p.File == file })
	if i < 0 {
		return m, ErrNotFound
	}
	p := m.Pages[i]
	m.Pages = slices.Delete(m.Pages, i, i+1)
	m.Updated = time.Now().UTC()
	// Manifest first: a crash leaves an orphan file, never a dangling entry.
	if err := s.writeManifest(m); err != nil {
		return m, err
	}
	s.removeFiles(s.dir(id), p)
	return m, nil
}

// Delete removes a whole session.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.load(id); err != nil {
		return err
	}
	if err := os.RemoveAll(s.dir(id)); err != nil {
		return err
	}
	return fsutil.SyncDir(s.root)
}

// DeleteIfUnchanged removes the session only if it was not modified after
// the manifest the caller worked from (updated = that manifest's Updated).
func (s *Store) DeleteIfUnchanged(id string, updated time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load(id)
	if err != nil {
		return err
	}
	if !m.Updated.Equal(updated) {
		return ErrChanged
	}
	if err := os.RemoveAll(s.dir(id)); err != nil {
		return err
	}
	return fsutil.SyncDir(s.root)
}

// CleanupAbandoned deletes sessions not updated for maxAge and returns
// their IDs. Sessions handed to the Paperless outbox are deleted at hand-off,
// so everything still here was never sent.
func (s *Store) CleanupAbandoned(maxAge time.Duration, now time.Time) ([]string, error) {
	list, err := s.List()
	if err != nil {
		return nil, err
	}
	var gone []string
	for _, m := range list {
		if now.Sub(m.Updated) < maxAge {
			continue
		}
		if err := s.Delete(m.ID); err != nil && !errors.Is(err, ErrNotFound) {
			return gone, err
		}
		gone = append(gone, m.ID)
	}
	return gone, nil
}

// PagePath and ThumbPath return on-disk paths for a page of session id.
func (s *Store) PagePath(id string, p Page) string  { return filepath.Join(s.dir(id), p.File) }
func (s *Store) ThumbPath(id string, p Page) string { return filepath.Join(s.dir(id), p.Thumb) }

func (s *Store) dir(id string) string { return filepath.Join(s.root, id) }

func (s *Store) removeFiles(dir string, p Page) {
	os.Remove(filepath.Join(dir, p.File))
	if p.Thumb != "" {
		os.Remove(filepath.Join(dir, p.Thumb))
	}
}

func (s *Store) load(id string) (Manifest, error) {
	var m Manifest
	if !idPattern.MatchString(id) {
		return m, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(s.dir(id), manifestName))
	if errors.Is(err, os.ErrNotExist) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func (s *Store) writeManifest(m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(s.dir(m.ID), manifestName), b, 0o600)
}

// thumbnail returns a small JPEG preview, or nil if the page can't be decoded.
func (s *Store) thumbnail(data []byte) []byte {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	b := img.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 {
		return nil
	}
	w := min(s.opt.ThumbWidth, b.Dx())
	h := max(1, b.Dy()*w/b.Dx())
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 75}); err != nil {
		return nil
	}
	return out.Bytes()
}

func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func sniff(b []byte) (mime, ext string) {
	switch {
	case len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg", ".jpg"
	case len(b) > 4 && (string(b[:4]) == "II*\x00" || string(b[:4]) == "MM\x00*"):
		return "image/tiff", ".tif"
	}
	return "", ""
}

// jfifDPI reads the density from a JFIF APP0 segment, if present.
func jfifDPI(b []byte) int {
	if len(b) < 18 || b[2] != 0xFF || b[3] != 0xE0 || string(b[6:11]) != "JFIF\x00" {
		return 0
	}
	x := int(binary.BigEndian.Uint16(b[14:16]))
	switch b[13] {
	case 1:
		return x
	case 2:
		return int(float64(x)*2.54 + 0.5)
	}
	return 0
}

// tiffDPI reads XResolution (tag 282) and ResolutionUnit (296) from the
// first IFD.
func tiffDPI(b []byte) int {
	if len(b) < 8 {
		return 0
	}
	var bo binary.ByteOrder = binary.LittleEndian
	if b[0] == 'M' {
		bo = binary.BigEndian
	}
	off := int(bo.Uint32(b[4:8]))
	if off < 8 || off+2 > len(b) {
		return 0
	}
	n := int(bo.Uint16(b[off:]))
	unit := 2 // inch (TIFF default)
	var num, den uint32
	for i := range n {
		e := off + 2 + 12*i
		if e+12 > len(b) {
			return 0
		}
		switch bo.Uint16(b[e:]) {
		case 282: // RATIONAL, stored at an offset
			vo := int(bo.Uint32(b[e+8:]))
			if vo < 8 || vo+8 > len(b) {
				return 0
			}
			num, den = bo.Uint32(b[vo:]), bo.Uint32(b[vo+4:])
		case 296:
			unit = int(bo.Uint16(b[e+8:]))
		}
	}
	if den == 0 {
		return 0
	}
	v := float64(num) / float64(den)
	if unit == 3 { // centimetre
		v *= 2.54
	}
	return int(v + 0.5)
}
