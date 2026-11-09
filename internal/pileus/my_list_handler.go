package pileus

import (
	"context"
	"log"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"mycelium/internal/managers"
)

// myListOwner is whose list a call reads/writes: the profile, or the device
// for a client with no profile — the same rule as Continue Watching.
func myListOwner(ctx context.Context) (string, error) {
	owner, _ := ctx.Value(ctxKeyDeviceID{}).(string)
	if owner == "" {
		owner = "unknown"
	}
	if pid := profileIDFromCtx(ctx); pid != "" {
		if !profileExists(pid) {
			return "", status.Error(codes.NotFound, "unknown profile")
		}
		owner = pid
	}
	return owner, nil
}

func (h *MediaHandler) GetMyList(ctx context.Context, req *gen.MyListRequest) (*gen.MyListResponse, error) {
	owner, err := myListOwner(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := managers.DB.GetMyList(owner, req.PluginId)
	if err != nil {
		log.Printf("[pileus/media] GetMyList: %v", err)
		return nil, status.Error(codes.Internal, "lettura della lista non riuscita")
	}
	items := make([]*gen.MyListItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, &gen.MyListItem{
			PluginId:  e.PluginID,
			MediaId:   e.MediaID,
			Title:     e.Title,
			PosterUrl: e.PosterURL,
			MediaType: e.MediaType,
			Year:      e.Year,
			Rating:    e.Rating,
			FanartUrl: e.FanartURL,
			LogoUrl:   e.LogoURL,
			AddedAt:   e.AddedAt,
			Extra:     e.Extra,
		})
	}
	return &gen.MyListResponse{Items: items}, nil
}

func (h *MediaHandler) SetMyListItem(ctx context.Context, req *gen.SetMyListItemRequest) (*gen.SetMyListItemResponse, error) {
	it := req.GetItem()
	if it.GetPluginId() == "" || it.GetMediaId() == "" {
		return nil, status.Error(codes.InvalidArgument, "plugin_id e media_id obbligatori")
	}
	owner, err := myListOwner(ctx)
	if err != nil {
		return nil, err
	}
	if req.InList {
		err = managers.DB.AddToMyList(owner, managers.MyListEntry{
			PluginID:  it.PluginId,
			MediaID:   it.MediaId,
			Title:     it.Title,
			PosterURL: it.PosterUrl,
			MediaType: it.MediaType,
			Year:      it.Year,
			Rating:    it.Rating,
			FanartURL: it.FanartUrl,
			LogoURL:   it.LogoUrl,
			Extra:     it.Extra,
		})
	} else {
		err = managers.DB.RemoveFromMyList(owner, it.PluginId, it.MediaId)
	}
	if err != nil {
		log.Printf("[pileus/media] SetMyListItem: %v", err)
		return nil, status.Error(codes.Internal, "aggiornamento della lista non riuscito")
	}
	return &gen.SetMyListItemResponse{Ok: true}, nil
}
