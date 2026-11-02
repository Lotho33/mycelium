package pileus

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"mycelium/internal/core"
)

func TestAbsolutizeServerURLs(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-http-host", "media.example.com", "x-http-scheme", "https"))
	cover := core.VersusCoverURL("https://cdn/a.png", "https://cdn/b.png")
	orig := &gen.CatalogResponse{Items: []*gen.CatalogItem{
		{Id: "1", PosterUrl: cover, Extra: map[string]string{"fanart_url": cover, "plot": "x"}},
		{Id: "2", PosterUrl: "https://cdn/p.jpg"},
	}}

	out := absolutizeServerURLs(ctx, orig).(*gen.CatalogResponse)
	// Same base as the /proxy URLs this client gets.
	scheme, host := clientProxyBase(ctx)
	want := scheme + "://" + host + "/img/versus?" + cover[len(core.ServerURLPrefix+"img/versus?"):]
	if scheme != "https" {
		t.Fatalf("scheme %q, want the client hint", scheme)
	}
	if got := out.Items[0].PosterUrl; got != want {
		t.Fatalf("poster = %q, want %q", got, want)
	}
	if got := out.Items[0].Extra["fanart_url"]; got != want {
		t.Fatalf("extra fanart = %q", got)
	}
	if out.Items[1].PosterUrl != "https://cdn/p.jpg" || out.Items[0].Extra["plot"] != "x" {
		t.Fatal("other strings changed")
	}
	// The original (possibly a cached response) keeps the relative form.
	if orig.Items[0].PosterUrl != cover || orig.Items[0].Extra["fanart_url"] != cover {
		t.Fatal("original message was mutated")
	}

	plain := &gen.CatalogResponse{Items: []*gen.CatalogItem{{Id: "3", PosterUrl: "https://cdn/p.jpg"}}}
	if got := absolutizeServerURLs(ctx, plain); got != proto.Message(plain) {
		t.Fatal("a message without server URLs must go out as is, not cloned")
	}
}
