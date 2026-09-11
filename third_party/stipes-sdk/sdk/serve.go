package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc"
)

const magicEnvKey = "FUNGOBOX_MAGIC"
const magicEnvVal = ProtocolVersion

// ServeProvider starts a gRPC server for the given Provider implementation.
// It writes {"port": N, "id": "<pluginID>", "type": "provider", "ready": true}
// to stdout so the FunGoBox core can connect.
func ServeProvider(pluginID string, impl Provider) {
	if os.Getenv(magicEnvKey) != magicEnvVal {
		fmt.Fprintln(os.Stderr, "[fungobox-sdk] missing magic env — run via FunGoBox core")
		os.Exit(1)
	}
	srv := grpc.NewServer()
	gen.RegisterProviderServer(srv, &providerAdapter{id: pluginID, impl: impl})
	serve(pluginID, "provider", srv)
}

// ServeEnricher starts a gRPC server for the given Enricher implementation.
func ServeEnricher(pluginID string, impl Enricher) {
	if os.Getenv(magicEnvKey) != magicEnvVal {
		fmt.Fprintln(os.Stderr, "[fungobox-sdk] missing magic env — run via FunGoBox core")
		os.Exit(1)
	}
	srv := grpc.NewServer()
	gen.RegisterEnricherServer(srv, &enricherAdapter{id: pluginID, impl: impl})
	serve(pluginID, "enricher", srv)
}

// serve binds a random port, announces it, then blocks until signal.
func serve(pluginID, pluginType string, srv *grpc.Server) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[fungobox-sdk] listen: %v\n", err)
		os.Exit(1)
	}

	port := lis.Addr().(*net.TCPAddr).Port
	announce(pluginID, pluginType, port)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()

	select {
	case sig := <-quit:
		fmt.Fprintf(os.Stderr, "[fungobox-sdk] received %s — shutting down\n", sig)
		srv.GracefulStop()
	case err := <-serveErr:
		if err != nil {
			fmt.Fprintf(os.Stderr, "[fungobox-sdk] serve error: %v\n", err)
			os.Exit(1)
		}
	}
}

// announce writes the startup JSON line that the FunGoBox core reads from stdout.
func announce(id, pluginType string, port int) {
	msg, _ := json.Marshal(map[string]any{
		"id":    id,
		"type":  pluginType,
		"port":  port,
		"ready": true,
	})
	fmt.Println(string(msg))
}

// ─────────────────────────────────────────────────────────────────────────────
// Proto ↔ SDK type conversions (internal to this package)
// ─────────────────────────────────────────────────────────────────────────────

func metaToProto(m *Meta) *gen.CatalogItem {
	if m == nil {
		return nil
	}
	extra := m.Metadata
	if m.Background != "" {
		if extra == nil {
			extra = make(map[string]string)
		}
		extra["fanart_url"] = m.Background
	}
	return &gen.CatalogItem{
		Id:              m.ID,
		Title:           m.Name,
		ProviderId:      m.ProviderID,
		MediaType:       m.Type,
		PosterUrl:       m.Poster,
		Year:            m.Year,
		Rating:          metaRatingFloat(m.IMDBRating),
		Extra:           extra,
		IsDir:           m.IsDir,
		SeasonNumber:    m.SeasonNumber,
		EpisodeNumber:   m.EpisodeNumber,
		IsExternal:      m.IsExternal,
		DirectStreamUrl: m.DirectStreamURL,
		ParentId:        m.ParentID,
		ShowId:          m.ShowID,
		LogoUrl:         m.LogoURL,
		BannerUrl:       m.BannerURL,
	}
}

func metasToProto(items []*Meta) []*gen.CatalogItem {
	out := make([]*gen.CatalogItem, len(items))
	for i, m := range items {
		out[i] = metaToProto(m)
	}
	return out
}

func metaFromProto(p *gen.CatalogItem) *Meta {
	if p == nil {
		return nil
	}
	rating := ""
	if p.Rating > 0 {
		rating = fmt.Sprintf("%.1f", p.Rating)
	}
	meta := p.Extra
	background := ""
	if meta != nil {
		background = meta["fanart_url"]
	}
	return &Meta{
		ID:              p.Id,
		Type:            protoTypeToStremio(p.MediaType),
		Name:            p.Title,
		Poster:          p.PosterUrl,
		Background:      background,
		Year:            p.Year,
		IMDBRating:      rating,
		SeasonNumber:    p.SeasonNumber,
		EpisodeNumber:   p.EpisodeNumber,
		IsDir:           p.IsDir,
		IsExternal:      p.IsExternal,
		DirectStreamURL: p.DirectStreamUrl,
		ParentID:        p.ParentId,
		ShowID:          p.ShowId,
		ProviderID:      p.ProviderId,
		Metadata:        meta,
		LogoURL:         p.LogoUrl,
		BannerURL:       p.BannerUrl,
	}
}

func metasFromProto(items []*gen.CatalogItem) []*Meta {
	out := make([]*Meta, len(items))
	for i, p := range items {
		out[i] = metaFromProto(p)
	}
	return out
}

func catalogToProto(c *Catalog) *gen.Section {
	if c == nil {
		return nil
	}
	previews := make([]*gen.CatalogItem, len(c.Preview))
	for i := range c.Preview {
		previews[i] = metaToProto(&c.Preview[i])
	}
	return &gen.Section{
		Id:           c.ID,
		Label:        c.Name,
		Icon:         c.Icon,
		MediaType:    c.Type,
		PreviewItems: previews,
	}
}

func catalogsToProto(catalogs []*Catalog) []*gen.Section {
	out := make([]*gen.Section, len(catalogs))
	for i, c := range catalogs {
		out[i] = catalogToProto(c)
	}
	return out
}

func streamToProto(s *Stream) *gen.ProviderResolveResponse {
	if s == nil {
		return &gen.ProviderResolveResponse{}
	}
	return &gen.ProviderResolveResponse{
		StreamUrl:   s.URL,
		Headers:     s.Headers,
		IsLive:      s.IsLive,
		IsResumable: s.IsResumable,
		Metadata:    s.Metadata,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Provider adapter: bridges gen.ProviderServer → sdk.Provider
// ─────────────────────────────────────────────────────────────────────────────

type providerAdapter struct {
	gen.UnimplementedProviderServer
	id   string
	impl Provider
}

func (a *providerAdapter) Init(_ context.Context, req *gen.InitRequest) (*gen.InitResponse, error) {
	if err := a.impl.Init(req.Settings); err != nil {
		return &gen.InitResponse{Ok: false, Error: err.Error()}, nil
	}
	return &gen.InitResponse{Ok: true}, nil
}

func (a *providerAdapter) Setup(_ context.Context, _ *gen.SetupRequest) (*gen.SetupResponse, error) {
	info, err := a.impl.Setup()
	if err != nil {
		return &gen.SetupResponse{Ready: false, Error: err.Error()}, nil
	}
	return &gen.SetupResponse{
		Ready:        info.Ready,
		Error:        info.Error,
		Capabilities: info.Capabilities,
	}, nil
}

// SyncCatalog is kept for backward compatibility — delegates to Tick("sync_catalog").
func (a *providerAdapter) SyncCatalog(ctx context.Context, _ *gen.SyncCatalogRequest) (*gen.SyncCatalogResponse, error) {
	if err := a.impl.Tick(ctx, "sync_catalog"); err != nil {
		return &gen.SyncCatalogResponse{Ok: false, Error: err.Error()}, nil
	}
	return &gen.SyncCatalogResponse{Ok: true}, nil
}

func (a *providerAdapter) Tick(ctx context.Context, req *gen.TickRequest) (*gen.TickResponse, error) {
	if err := a.impl.Tick(ctx, req.GetJobId()); err != nil {
		return &gen.TickResponse{Ok: false, Error: err.Error()}, nil
	}
	return &gen.TickResponse{Ok: true}, nil
}

func (a *providerAdapter) GetSections(ctx context.Context, _ *gen.GetSectionsRequest) (*gen.GetSectionsResponse, error) {
	catalogs, err := a.impl.GetSections(ctx)
	if err != nil {
		return nil, gen.WrapError(err)
	}
	return &gen.GetSectionsResponse{Sections: catalogsToProto(catalogs)}, nil
}

func (a *providerAdapter) Search(ctx context.Context, req *gen.ProviderSearchRequest) (*gen.ProviderSearchResponse, error) {
	items, err := a.impl.Search(ctx, req.Query, int(req.Year), req.Genre, int(req.Page))
	if err != nil {
		return nil, gen.WrapError(err)
	}
	return &gen.ProviderSearchResponse{Items: metasToProto(items)}, nil
}

func (a *providerAdapter) Browse(ctx context.Context, req *gen.ProviderBrowseRequest) (*gen.ProviderBrowseResponse, error) {
	page := int(req.Page)
	if page < 1 {
		page = 1
	}
	items, err := a.impl.Browse(ctx, req.DirectoryId, page)
	if err != nil {
		return nil, gen.WrapError(err)
	}
	return &gen.ProviderBrowseResponse{Items: metasToProto(items)}, nil
}

func (a *providerAdapter) Resolve(ctx context.Context, req *gen.ProviderResolveRequest) (*gen.ProviderResolveResponse, error) {
	stream, err := a.impl.Resolve(ctx, req.PlayableId)
	if err != nil {
		return nil, gen.WrapError(err)
	}
	return streamToProto(stream), nil
}

func (a *providerAdapter) GetMeta(ctx context.Context, req *gen.GetMetaRequest) (*gen.GetMetaResponse, error) {
	item, err := a.impl.GetMeta(ctx, req.ItemId)
	if err != nil {
		return nil, gen.WrapError(err)
	}
	return &gen.GetMetaResponse{Item: metaToProto(item)}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Enricher adapter: bridges gen.EnricherServer → sdk.Enricher
// ─────────────────────────────────────────────────────────────────────────────

type enricherAdapter struct {
	gen.UnimplementedEnricherServer
	id   string
	impl Enricher
}

func (a *enricherAdapter) Init(_ context.Context, req *gen.InitRequest) (*gen.InitResponse, error) {
	if err := a.impl.Init(req.Settings); err != nil {
		return &gen.InitResponse{Ok: false, Error: err.Error()}, nil
	}
	return &gen.InitResponse{Ok: true}, nil
}

func (a *enricherAdapter) Setup(_ context.Context, _ *gen.SetupRequest) (*gen.SetupResponse, error) {
	info, err := a.impl.Setup()
	if err != nil {
		return &gen.SetupResponse{Ready: false, Error: err.Error()}, nil
	}
	return &gen.SetupResponse{
		Ready:        info.Ready,
		Error:        info.Error,
		Capabilities: info.Capabilities,
	}, nil
}

func (a *enricherAdapter) Enrich(ctx context.Context, req *gen.EnrichRequest) (*gen.EnrichResponse, error) {
	items, err := a.impl.Enrich(ctx, metasFromProto(req.Items))
	if err != nil {
		return nil, gen.WrapError(err)
	}
	return &gen.EnrichResponse{Items: metasToProto(items)}, nil
}
