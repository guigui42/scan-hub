// Package pipeline hands finished sessions to the Paperless outbox.
package pipeline

import (
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/guigui42/scan-hub/internal/paperless"
	"github.com/guigui42/scan-hub/internal/pdf"
	"github.com/guigui42/scan-hub/internal/session"
)

// ErrEmpty means the session has no pages.
var ErrEmpty = errors.New("session has no pages")

// PDFPages maps a session's pages to PDF builder inputs.
func PDFPages(store *session.Store, m session.Manifest) []pdf.Page {
	pages := make([]pdf.Page, 0, len(m.Pages))
	for _, p := range m.Pages {
		pages = append(pages, pdf.Page{Path: store.PagePath(m.ID, p), MIME: p.MIME, DPI: p.DPI})
	}
	return pages
}

// Submit merges session id into one PDF in the outbox, then deletes the
// session: once Enqueue returns, the outbox owns the document.
func Submit(log *slog.Logger, store *session.Store, ob *paperless.Outbox, id string) (paperless.Item, error) {
	m, err := store.Get(id)
	if err != nil {
		return paperless.Item{}, fmt.Errorf("session %s: %w", id, err)
	}
	if len(m.Pages) == 0 {
		return paperless.Item{}, ErrEmpty
	}
	it, err := ob.Enqueue(paperless.EnqueueMeta{Title: m.Title, Source: m.Source, Pages: len(m.Pages)}, func(w io.Writer) error {
		return pdf.Write(w, PDFPages(store, m), pdf.Options{Title: m.Title, Created: m.Created})
	})
	if err != nil {
		return it, err
	}
	// Pages added while the PDF was built are not in it: keep the session.
	if err := store.DeleteIfUnchanged(id, m.Updated); err != nil {
		log.Warn("session kept after hand-off to outbox", "session", id, "err", err)
	}
	log.Info("session sent to outbox", "session", id, "outbox", it.ID, "pages", len(m.Pages))
	return it, nil
}
