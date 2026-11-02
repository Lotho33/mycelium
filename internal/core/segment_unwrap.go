package core

import (
	"fmt"
	"strings"
)

// SegmentUnwrap is one rule a plugin declares (manifest `segments.unwrap`)
// for media segments its source serves inside another container. The proxy
// knows only the carriers and codecs below; every site-specific byte (magic
// strings, chunk names) comes from the plugin.
//
//	carrier  where the payload sits:
//	           body        anywhere in the response, right after Magic
//	           png_pixels  in the samples of an 8-bit RGB/RGBA PNG (alpha
//	                       dropped), read row by row from the first pixel
//	           webp_chunk  in the data of the WebP chunk named Chunk
//	chunk    webp_chunk only: the chunk's FourCC (e.g. "EXIF")
//	magic    bytes right before the payload; required for body, which is
//	         searched for it, and must start the carrier otherwise
//	length   "" (payload runs to the end) | u32be | u32le — a length field
//	         right after Magic
//	codec    "" (raw) | gzip | zlib
//
// Whatever a rule extracts must be MPEG-TS or fMP4, or it is ignored.
type SegmentUnwrap struct {
	Carrier string `yaml:"carrier" json:"carrier"`
	Chunk   string `yaml:"chunk" json:"chunk,omitempty"`
	Magic   string `yaml:"magic" json:"magic,omitempty"`
	Length  string `yaml:"length" json:"length,omitempty"`
	Codec   string `yaml:"codec" json:"codec,omitempty"`
}

const (
	UnwrapCarrierBody      = "body"
	UnwrapCarrierPNGPixels = "png_pixels"
	UnwrapCarrierWebPChunk = "webp_chunk"

	// MaxSegmentUnwrapRules bounds the rules tried on one segment.
	MaxSegmentUnwrapRules = 8
	maxUnwrapMagic        = 64
)

// Validate reports why a rule can't be used (nil = valid).
func (u SegmentUnwrap) Validate() error {
	switch u.Carrier {
	case UnwrapCarrierBody:
		if u.Magic == "" {
			return fmt.Errorf("carrier body richiede magic")
		}
	case UnwrapCarrierPNGPixels:
	case UnwrapCarrierWebPChunk:
		if len(u.Chunk) != 4 {
			return fmt.Errorf("carrier webp_chunk richiede chunk di 4 caratteri, non %q", u.Chunk)
		}
	default:
		return fmt.Errorf("carrier %q sconosciuto (body | png_pixels | webp_chunk)", u.Carrier)
	}
	if len(u.Magic) > maxUnwrapMagic {
		return fmt.Errorf("magic oltre %d byte", maxUnwrapMagic)
	}
	switch u.Length {
	case "", "u32be", "u32le":
	default:
		return fmt.Errorf("length %q sconosciuto (u32be | u32le)", u.Length)
	}
	switch strings.ToLower(u.Codec) {
	case "", "gzip", "zlib":
	default:
		return fmt.Errorf("codec %q sconosciuto (gzip | zlib)", u.Codec)
	}
	return nil
}

// ValidSegmentUnwraps keeps the valid rules (at most MaxSegmentUnwrapRules)
// and describes the dropped ones.
func ValidSegmentUnwraps(rules []SegmentUnwrap) (valid []SegmentUnwrap, problems []string) {
	for i, r := range rules {
		if err := r.Validate(); err != nil {
			problems = append(problems, fmt.Sprintf("segments.unwrap[%d]: %v", i, err))
			continue
		}
		if len(valid) == MaxSegmentUnwrapRules {
			problems = append(problems, fmt.Sprintf("segments.unwrap: oltre %d regole, le altre ignorate", MaxSegmentUnwrapRules))
			break
		}
		r.Codec = strings.ToLower(r.Codec)
		valid = append(valid, r)
	}
	return valid, problems
}
