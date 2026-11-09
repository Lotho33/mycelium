package api

// Profile pictures set from the dashboard. The image is cropped to a square,
// scaled down and stored under data/avatars; the profile's avatar_url
// becomes a server-relative URL (mycelium:/profile-avatar/<id>?v=…), which
// the gRPC layer turns into an absolute one for each client (server_urls.go),
// so Pileus shows it like any other image URL. A client that later picks one
// of its built-in avatars simply replaces the URL.

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // decoders for uploaded pictures
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	_ "image/gif"

	"golang.org/x/image/draw"

	"mycelium/internal/core"
	"mycelium/internal/pileus"
)

const (
	avatarSize          = 256
	maxAvatarUpload     = 10 << 20
	maxAvatarPixels     = 40_000_000
	avatarJPEGQuality   = 86
	profileAvatarPrefix = core.ServerURLPrefix + "profile-avatar/"
)

var profileIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func avatarPath(profileID string) string {
	return core.AppPath("data", "avatars", profileID+".jpg")
}

// RemoveProfileAvatar deletes a profile's stored picture, if any.
func RemoveProfileAvatar(profileID string) {
	if profileIDRe.MatchString(profileID) {
		_ = os.Remove(avatarPath(profileID))
	}
}

// squareThumb crops src to its centred square and scales it to size×size,
// on an opaque background (JPEG has no alpha).
func squareThumb(src image.Image, size int) image.Image {
	b := src.Bounds()
	side := min(b.Dx(), b.Dy())
	crop := image.Rect(0, 0, side, side).Add(image.Pt(b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2))
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{color.RGBA{0x1f, 0x24, 0x30, 0xff}}, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, crop, draw.Over, nil)
	return dst
}

// encodeAvatar decodes an uploaded picture and returns the stored JPEG.
func encodeAvatar(data []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("formato immagine non supportato (usa JPEG, PNG, WebP o GIF)")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxAvatarPixels {
		return nil, errors.New("immagine troppo grande")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("immagine illeggibile")
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, squareThumb(img, avatarSize), &jpeg.Options{Quality: avatarJPEGQuality}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func findProfile(id string) (*pileus.AdminProfileInfo, error) {
	list, err := pileus.ListProfilesAdmin()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ProfileID == id {
			return &list[i], nil
		}
	}
	return nil, nil
}

// POST /admin/pileus/profiles/{id}/avatar (multipart "image") → {avatar_url}
func uploadProfileAvatar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := findProfile(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if p == nil || !profileIDRe.MatchString(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "profilo non trovato"})
		return
	}
	file, _, err := r.FormFile("image")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "nessuna immagine ricevuta"})
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAvatarUpload+1))
	if err != nil || len(data) > maxAvatarUpload {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "immagine oltre 10 MB"})
		return
	}
	jpg, err := encodeAvatar(data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	dest := avatarPath(id)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, jpg, 0o644); err != nil || os.Rename(tmp, dest) != nil {
		os.Remove(tmp)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": "salvataggio immagine non riuscito"})
		return
	}
	// ?v= changes with every picture, so clients don't keep the old one cached.
	url := profileAvatarPrefix + id + "?v=" + strconv.FormatInt(time.Now().UnixMilli(), 36)
	if err := pileus.UpdateProfileAdmin(id, p.Name, url); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	log.Printf("[profiles] foto aggiornata per il profilo %s", id)
	writeJSON(w, http.StatusOK, map[string]any{"avatar_url": url})
}

// DELETE /admin/pileus/profiles/{id}/avatar — back to the client's default.
func deleteProfileAvatar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := findProfile(id)
	if err != nil || p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "profilo non trovato"})
		return
	}
	RemoveProfileAvatar(id)
	if err := pileus.UpdateProfileAdmin(id, p.Name, ""); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /profile-avatar/{id} — public like the other images the players load
// (/img, /plugin-icon); profile ids are random 128-bit values.
func serveProfileAvatar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !profileIDRe.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(avatarPath(id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	// The URL changes (?v=) with every new picture.
	w.Header().Set("Cache-Control", "public, max-age=604800")
	http.ServeContent(w, r, "", st.ModTime(), f)
}
