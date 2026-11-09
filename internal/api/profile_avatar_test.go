package api

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// A wide picture is cropped to its centre and scaled to avatarSize.
func TestEncodeAvatar_CropsCentreAndScales(t *testing.T) {
	// 600×300: red left third, green middle, blue right third.
	src := image.NewRGBA(image.Rect(0, 0, 600, 300))
	for x := 0; x < 600; x++ {
		c := color.RGBA{255, 0, 0, 255}
		if x >= 200 && x < 400 {
			c = color.RGBA{0, 255, 0, 255}
		} else if x >= 400 {
			c = color.RGBA{0, 0, 255, 255}
		}
		for y := 0; y < 300; y++ {
			src.Set(x, y, c)
		}
	}
	var in bytes.Buffer
	if err := png.Encode(&in, src); err != nil {
		t.Fatal(err)
	}
	out, err := encodeAvatar(in.Bytes())
	if err != nil {
		t.Fatalf("encodeAvatar: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != avatarSize || b.Dy() != avatarSize {
		t.Fatalf("size %v, want %d×%d", b, avatarSize, avatarSize)
	}
	// The centre square (x 150-450) is mostly green.
	r, g, _, _ := img.At(avatarSize/2, avatarSize/2).RGBA()
	if g < 0xc000 || r > 0x4000 {
		t.Fatalf("centre pixel not green: r=%x g=%x", r, g)
	}
}

func TestEncodeAvatar_RejectsNonImages(t *testing.T) {
	if _, err := encodeAvatar([]byte("not an image")); err == nil {
		t.Fatal("garbage accepted")
	}
}
