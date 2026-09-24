package api

import (
	"context"
	"net/http"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/tracking"
)

var pixelGIF = []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0x21, 0xf9, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00, 0x2c, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x44, 0x01, 0x00, 0x3b}

type trackHandler struct {
	db     *database.DB
	secret []byte
}

func (h *trackHandler) handleOpen(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	p, err := tracking.Verify(h.secret, token)
	if err == nil {
		_, _ = h.db.AppendEvent(context.Background(), p.TenantID, p.MessageID, database.EventOpened, map[string]any{"recipient": p.Recipient})
	}
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pixelGIF)
}

func (h *trackHandler) handleClick(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	p, err := tracking.Verify(h.secret, token)
	if err != nil || p.URL == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_, _ = h.db.AppendEvent(context.Background(), p.TenantID, p.MessageID, database.EventClicked, map[string]any{"recipient": p.Recipient, "url": p.URL})
	http.Redirect(w, r, p.URL, http.StatusFound)
}
