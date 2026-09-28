package server

import (
	"bytes"
	"errors"
	"image"
	_ "image/jpeg" // registers the decoder DecodeConfig uses
	_ "image/png"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/TheWarBoys2/pzadmin/internal/fsutil"
)

// The world map is one image the operator uploads, shared by every server:
// vanilla coordinates are the same on every server, and a modded map only
// adds areas beyond them. It is lined up with the game by two points, each an
// in-game tile coordinate and the image pixel that shows it. The Live tab
// draws player pins from that.
const (
	worldMapMaxBytes = 25 << 20
	// A map wider or taller than this is too heavy for a browser to pan.
	worldMapMaxSide = 16384
)

// mapPoint ties an in-game tile (X, Y) to an image pixel (PX, PY).
type mapPoint struct {
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	PX float64 `json:"px"`
	PY float64 `json:"py"`
}

type worldMapMeta struct {
	Type      string       `json:"type"`
	Width     int          `json:"width"`
	Height    int          `json:"height"`
	Bytes     int64        `json:"bytes"`
	UpdatedAt OptionalTime `json:"updatedAt"`
	// Points is empty until the map has been lined up with the game.
	Points []mapPoint `json:"points"`
}

func (a *App) worldMapPaths() (img, meta string) {
	dir := filepath.Join(a.dataDir, "worldmap")
	return filepath.Join(dir, "map.img"), filepath.Join(dir, "map.json")
}

func (a *App) worldMapMeta() (worldMapMeta, bool) {
	_, metaPath := a.worldMapPaths()
	var m worldMapMeta
	found, err := fsutil.ReadJSON(metaPath, &m)
	if err != nil || !found {
		return worldMapMeta{}, false
	}
	if m.Points == nil {
		m.Points = []mapPoint{}
	}
	return m, true
}

func (a *App) handleWorldMap(w http.ResponseWriter, r *http.Request) {
	m, found := a.worldMapMeta()
	if !found {
		writeJSON(w, map[string]any{"present": false})
		return
	}
	writeJSON(w, map[string]any{"present": true, "map": m})
}

func (a *App) handleWorldMapImage(w http.ResponseWriter, r *http.Request) {
	m, found := a.worldMapMeta()
	imgPath, _ := a.worldMapPaths()
	f, err := os.Open(imgPath)
	if !found || err != nil {
		httpError(w, http.StatusNotFound, "no map image has been uploaded")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", m.Type)
	// The URL carries the upload time, so a new upload is a new URL.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, "map", m.UpdatedAt.Time, f)
}

// handleWorldMapUpload takes the image as the raw request body.
func (a *App) handleWorldMapUpload(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, worldMapMaxBytes))
	if err != nil {
		httpError(w, http.StatusRequestEntityTooLarge, "the image is larger than 25 MB; save it smaller or as a JPEG")
		return
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") {
		httpError(w, http.StatusBadRequest, "that is not a JPEG or PNG image")
		return
	}
	if cfg.Width > worldMapMaxSide || cfg.Height > worldMapMaxSide {
		httpError(w, http.StatusBadRequest, "the image is "+strconv.Itoa(cfg.Width)+"×"+strconv.Itoa(cfg.Height)+
			" pixels; resize it so neither side is over "+strconv.Itoa(worldMapMaxSide)+", or browsers struggle to show it")
		return
	}
	imgPath, metaPath := a.worldMapPaths()
	if err := fsutil.WriteFile(imgPath, data, 0o600); err != nil {
		httpError(w, http.StatusInternalServerError, "could not save the image: "+err.Error())
		return
	}
	// A new image needs lining up again: the old points name its pixels.
	m := worldMapMeta{Type: "image/" + format, Width: cfg.Width, Height: cfg.Height,
		Bytes: int64(len(data)), UpdatedAt: TimeNow(), Points: []mapPoint{}}
	if err := fsutil.WriteJSON(metaPath, m, 0o600); err != nil {
		httpError(w, http.StatusInternalServerError, "could not save the map details: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"present": true, "map": m})
}

// checkMapPoints refuses two points that cannot fix both axes. The image is a
// top-down map, so each axis is its own straight line and needs the points
// apart in both directions, in the game and on the image.
func checkMapPoints(p []mapPoint, width, height int) error {
	if len(p) != 2 {
		return errors.New("line the map up with exactly two points")
	}
	for _, q := range p {
		if q.PX < 0 || q.PY < 0 || q.PX > float64(width) || q.PY > float64(height) {
			return errors.New("a point is outside the image")
		}
		for _, v := range []float64{q.X, q.Y, q.PX, q.PY} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return errors.New("a point is not a number")
			}
		}
	}
	dx, dy := math.Abs(p[1].X-p[0].X), math.Abs(p[1].Y-p[0].Y)
	dpx, dpy := math.Abs(p[1].PX-p[0].PX), math.Abs(p[1].PY-p[0].PY)
	if dx < 100 || dy < 100 {
		return errors.New("the two in-game spots must be at least 100 tiles apart both across and up-down; pick spots further apart, diagonally")
	}
	if dpx < 10 || dpy < 10 {
		return errors.New("the two spots are too close together on the image")
	}
	return nil
}

func (a *App) handleWorldMapCalibrate(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Points []mapPoint `json:"points"`
	}
	if !decodeJSON(w, r, &p) {
		return
	}
	m, found := a.worldMapMeta()
	if !found {
		httpError(w, http.StatusNotFound, "upload a map image first")
		return
	}
	if err := checkMapPoints(p.Points, m.Width, m.Height); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	m.Points = p.Points
	_, metaPath := a.worldMapPaths()
	if err := fsutil.WriteJSON(metaPath, m, 0o600); err != nil {
		httpError(w, http.StatusInternalServerError, "could not save the map details: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"present": true, "map": m})
}

func (a *App) handleWorldMapDelete(w http.ResponseWriter, r *http.Request) {
	imgPath, metaPath := a.worldMapPaths()
	for _, p := range []string{metaPath, imgPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			httpError(w, http.StatusInternalServerError, "could not remove the map: "+err.Error())
			return
		}
	}
	writeJSON(w, map[string]any{"present": false})
}
