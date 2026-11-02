package core

import "testing"

func TestValidSegmentUnwraps(t *testing.T) {
	rules := []SegmentUnwrap{
		{Carrier: "png_pixels", Magic: "M", Length: "u32be", Codec: "GZIP"},
		{Carrier: "body"},                           // body needs a magic
		{Carrier: "webp_chunk", Chunk: "EXIFX"},     // FourCC
		{Carrier: "body", Magic: "M", Codec: "br"},  // unknown codec
		{Carrier: "body", Magic: "M", Length: "u8"}, // unknown length
		{Carrier: "svg"},                            // unknown carrier
		{Carrier: "webp_chunk", Chunk: "EXIF"},
	}
	valid, problems := ValidSegmentUnwraps(rules)
	if len(valid) != 2 || len(problems) != 5 {
		t.Fatalf("valid=%d problems=%v", len(valid), problems)
	}
	if valid[0].Codec != "gzip" {
		t.Fatalf("codec not normalised: %q", valid[0].Codec)
	}
	many := make([]SegmentUnwrap, MaxSegmentUnwrapRules+3)
	for i := range many {
		many[i] = SegmentUnwrap{Carrier: "png_pixels"}
	}
	if valid, _ := ValidSegmentUnwraps(many); len(valid) != MaxSegmentUnwrapRules {
		t.Fatalf("kept %d rules, cap is %d", len(valid), MaxSegmentUnwrapRules)
	}
}
