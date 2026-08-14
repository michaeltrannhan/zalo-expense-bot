package gemini

import (
	"bytes"
	"image"
	"image/jpeg"
	_ "image/png"
)

// downscaleForOCR shrinks a receipt image to a Gemini-reasonable max edge
// and JPEG-compresses it. The original bytes stay in object storage; only
// the HTTP payload to generateContent is reduced. Undecodable bytes (mock
// fixtures, webp/heic) are left unchanged.
func downscaleForOCR(data []byte) (out []byte, mime string, ok bool) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", false
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 1 || h < 1 {
		return nil, "", false
	}
	scaled := img
	if w > ocrMaxEdge || h > ocrMaxEdge {
		nw, nh := w, h
		if w >= h {
			nw = ocrMaxEdge
			nh = h * ocrMaxEdge / w
		} else {
			nh = ocrMaxEdge
			nw = w * ocrMaxEdge / h
		}
		if nw < 1 {
			nw = 1
		}
		if nh < 1 {
			nh = 1
		}
		scaled = resizeNearest(img, nw, nh)
	} else if len(data) <= 512<<10 && w <= ocrMaxEdge && h <= ocrMaxEdge {
		// Already receipt-sized; skip a quality-losing re-encode.
		return nil, "", false
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: ocrJPEGQuality}); err != nil {
		return nil, "", false
	}
	if buf.Len() == 0 || buf.Len() >= len(data) && w <= ocrMaxEdge && h <= ocrMaxEdge {
		return nil, "", false
	}
	return buf.Bytes(), "image/jpeg", true
}

func resizeNearest(src image.Image, nw, nh int) image.Image {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		sy := sb.Min.Y + y*sh/nh
		for x := 0; x < nw; x++ {
			sx := sb.Min.X + x*sw/nw
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}
