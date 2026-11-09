package pileus

import (
	"testing"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/protobuf/proto"

	"mycelium/internal/logometa"
)

func TestFillLogoMeta(t *testing.T) {
	const known = "https://example.com/known-logo.png"
	logometa.Put(known, logometa.Meta{W: 300, H: 80, Mid: 0.02, Halo: "light"})

	resp := &gen.CatalogResponse{Items: []*gen.CatalogItem{
		{Id: "a", LogoUrl: known + "|800|213|1"}, // legacy suffix: same logo
		{Id: "b", LogoUrl: "https://example.com/not-measured-yet.png"},
		{Id: "c"},
	}}
	out := fillLogoMeta(resp).(*gen.CatalogResponse)

	if out == resp {
		t.Fatal("the response must be cloned before it is changed")
	}
	if resp.Items[0].LogoMeta != nil {
		t.Fatal("the original (possibly cached) response was modified")
	}
	if lm := out.Items[0].LogoMeta; lm == nil || lm.Halo != "light" || lm.Width != 300 || lm.Height != 80 {
		t.Errorf("item a meta = %v", lm)
	}
	if out.Items[1].LogoMeta != nil || out.Items[2].LogoMeta != nil {
		t.Error("meta set for a logo without measurements")
	}
	if out.Items[0].LogoUrl != known+"|800|213|1" {
		t.Error("logo_url must reach clients unchanged")
	}

	// Nested details and lists inside other messages.
	det := &gen.DetailsResponse{}
	if !proto.Equal(fillLogoMeta(det), det) {
		t.Error("empty details changed")
	}
}

func TestFillLogoMetaNoLogos(t *testing.T) {
	resp := &gen.CatalogResponse{Items: []*gen.CatalogItem{{Id: "x", Title: "no logo"}}}
	if out := fillLogoMeta(resp); out != proto.Message(resp) {
		t.Error("a response without measured logos must go out as is (no clone)")
	}
}
