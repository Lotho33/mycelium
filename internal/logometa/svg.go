package logometa

import (
	"bytes"
	"fmt"
	"image"
	"math"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

// SVG logos are measured on a raster drawn here: clients draw them as
// vectors (sharp at any size), so a plugin hands out the SVG itself rather
// than a bitmap made for the core. Only the measurement needs pixels.
const (
	maxSVGBytes = 1 << 20 // logos are a few KB; parsing is bounded by this
	svgRasterPx = 512     // longer side of the raster measured
)

// looksLikeSVG reports whether raw is an SVG document (the start of it:
// an optional XML prolog, comments or a doctype before <svg).
func looksLikeSVG(raw []byte) bool {
	head := raw
	if len(head) > 4096 {
		head = head[:4096]
	}
	return bytes.Contains(bytes.ToLower(head), []byte("<svg"))
}

// rasterizeSVG draws an SVG logo onto a transparent RGBA image whose longer
// side is svgRasterPx, keeping its aspect ratio. Nothing external is loaded
// (no images, fonts or stylesheets referenced by the document).
func rasterizeSVG(raw []byte) (image.Image, error) {
	if len(raw) > maxSVGBytes {
		return nil, fmt.Errorf("svg over %d KB", maxSVGBytes>>10)
	}
	icon, err := oksvg.ReadIconStream(bytes.NewReader(raw), oksvg.IgnoreErrorMode)
	if err != nil {
		return nil, fmt.Errorf("svg: %w", err)
	}
	vw, vh := icon.ViewBox.W, icon.ViewBox.H
	if !(vw > 0 && vh > 0) || math.IsInf(vw, 0) || math.IsInf(vh, 0) {
		return nil, fmt.Errorf("svg without a usable size")
	}
	w, h := svgRasterPx, svgRasterPx
	if vw >= vh {
		h = int(math.Max(1, math.Round(svgRasterPx*vh/vw)))
	} else {
		w = int(math.Max(1, math.Round(svgRasterPx*vw/vh)))
	}
	icon.SetTarget(0, 0, float64(w), float64(h))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	scanner := rasterx.NewScannerGV(w, h, img, img.Bounds())
	icon.Draw(rasterx.NewDasher(w, h, scanner), 1)
	return img, nil
}
