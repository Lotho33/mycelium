package pileus

import (
	"context"
	"errors"
	"strings"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"

	"mycelium/internal/downloads"
	"mycelium/internal/engine"
	"mycelium/internal/managers"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DownloadResolver resolves a stream for the download worker
// (downloads.Resolver). It runs the plugin exactly like ResolveStream and
// hands back a loopback /proxy URL — the downloader then fetches playlists,
// keys and segments through mycelium's own proxy, so the plugin's egress
// (VPN), headers/cookies, AES-128 keys and expired-token recovery all apply
// unchanged. direct_stream plugins bypass the proxy for playback, so they do
// here too.
func DownloadResolver(ctx context.Context, pluginID, streamID, owner string) (downloads.Source, error) {
	b, err := lookupBackend(pluginID)
	if err != nil {
		return downloads.Source{}, err
	}
	// The owner is a profile id, or a device id when the download was made
	// without one: only a real profile goes to the plugin (per-profile logins).
	profileID := ""
	if owner != "" && profileExists(owner) {
		profileID = owner
		ctx = context.WithValue(ctx, ctxKeyProfileID{}, owner)
	}
	rawURL, headers, isLive, extra, err := b.ResolveStream(ctx, streamID, nil)
	if err != nil {
		return downloads.Source{}, wrapInternal(err)
	}
	headers = canonicalHeaderKeys(headers)
	lower := strings.ToLower(rawURL)
	isHLS := strings.Contains(lower, ".m3u8") || strings.Contains(lower, "/playlist/")
	src := downloads.Source{IsHLS: isHLS, IsLive: isLive, Extra: extra}
	if engine.LuaPlugins.ShouldSkipProxy(pluginID) {
		src.URL, src.Headers = rawURL, headers
		return src, nil
	}
	port := managers.Settings.GetString("server_port", "8000")
	// A uid that is not a playback session (no lease, no "someone is
	// watching" pause) but names the owner profile, so the proxy re-resolves
	// an expired token with that profile's plugin login.
	uid := ""
	if profileID != "" {
		uid = "dl-" + newID()
		managers.ProxyOwners.Register(uid, profileID)
	}
	src.URL = mintProxyURL("http", "127.0.0.1:"+port, pluginID, streamID, rawURL, headers, uid, isHLS)
	return src, nil
}

// downloadOwner is who a download belongs to: the active profile, else the
// device itself.
func downloadOwner(ctx context.Context) (owner, device string) {
	device, _ = ctx.Value(ctxKeyDeviceID{}).(string)
	if pid := profileIDFromCtx(ctx); pid != "" {
		return pid, device
	}
	return device, device
}

func downloadErr(err error) error {
	var ua downloads.ErrUnavailable
	switch {
	case errors.As(err, &ua):
		return status.Error(codes.FailedPrecondition, ua.Reason)
	case downloads.IsNotFound(err):
		return status.Error(codes.NotFound, err.Error())
	}
	return wrapInternal(err)
}

func requireDownloads() error {
	if downloads.M == nil {
		return status.Error(codes.Unavailable, "i download non sono attivi su questo server")
	}
	return nil
}

func (h *MediaHandler) GetDownloadOptions(ctx context.Context, req *gen.DownloadOptionsRequest) (*gen.DownloadOptionsResponse, error) {
	if err := requireDownloads(); err != nil {
		return nil, err
	}
	if req.PluginId == "" || req.StreamId == "" {
		return nil, status.Error(codes.InvalidArgument, "plugin_id e stream_id obbligatori")
	}
	owner, _ := downloadOwner(ctx)
	o, err := downloads.M.Options(ctx, req.PluginId, req.StreamId, owner)
	if err != nil {
		return nil, downloadErr(err)
	}
	resp := &gen.DownloadOptionsResponse{
		Available:         o.Available,
		UnavailableReason: o.Reason,
		DurationSec:       o.DurationSec,
		PluginNote:        o.PluginNote,
		PluginNotice:      o.PluginNotice,
		PreferredHours:    o.PreferredHours,
		QualityBelowUsual: o.QualityBelowUsual,
		UsualMaxHeight:    int32(o.UsualMaxHeight),
		ServerFreeBytes:   o.FreeBytes,
		BytesPerSec:       o.BytesPerSec,
		RetentionDays:     int32(o.RetentionDays),

		PreferredHoursSource: o.PreferredHoursSource,
	}
	for _, v := range o.Variants {
		resp.Variants = append(resp.Variants, &gen.DownloadVariant{
			Id: v.ID, Width: int32(v.Width), Height: int32(v.Height), Bandwidth: v.Bandwidth,
			BandwidthIsPeak: v.Peak, Codecs: v.Codecs, EstimatedBytes: v.EstimatedBytes, Label: v.Label,
		})
	}
	track := func(t downloads.OptTrack) *gen.DownloadTrack {
		return &gen.DownloadTrack{Id: t.ID, Language: t.Language, Name: t.Name, IsDefault: t.Default, EstimatedBytes: t.EstimatedBytes}
	}
	for _, t := range o.Audio {
		resp.Audio = append(resp.Audio, track(t))
	}
	for _, t := range o.Subtitles {
		resp.Subtitles = append(resp.Subtitles, track(t))
	}
	return resp, nil
}

func (h *MediaHandler) CreateDownload(ctx context.Context, req *gen.CreateDownloadRequest) (*gen.DownloadInfo, error) {
	if err := requireDownloads(); err != nil {
		return nil, err
	}
	if req.PluginId == "" || req.StreamId == "" {
		return nil, status.Error(codes.InvalidArgument, "plugin_id e stream_id obbligatori")
	}
	owner, device := downloadOwner(ctx)
	in, err := downloads.M.Create(ctx, downloads.CreateRequest{
		Owner: owner, Device: device, PluginID: req.PluginId,
		ItemMeta: downloads.ItemMeta{
			StreamID: req.StreamId, MediaID: req.MediaId, ParentID: req.ParentId, Title: req.Title,
			SeriesTitle: req.SeriesTitle, Poster: req.Poster,
			SeasonNumber: req.SeasonNumber, EpisodeNumber: req.EpisodeNumber,
		},
		VariantID: req.VariantId, AudioIDs: req.AudioIds, SubtitleIDs: req.SubtitleIds,
		NotBefore: req.NotBefore, UsePreferredHours: req.UsePreferredHours, UpgradeIfBetter: req.UpgradeIfBetter,
	})
	if err != nil {
		return nil, downloadErr(err)
	}
	scheme, host := clientProxyBase(ctx)
	return downloadInfoProto(in, scheme+"://"+host), nil
}

// CreateDownloads queues several streams of one plugin (a whole season) with
// one choice of quality/tracks.
func (h *MediaHandler) CreateDownloads(ctx context.Context, req *gen.CreateDownloadsRequest) (*gen.CreateDownloadsResponse, error) {
	if err := requireDownloads(); err != nil {
		return nil, err
	}
	if req.PluginId == "" || len(req.Items) == 0 {
		return nil, status.Error(codes.InvalidArgument, "plugin_id e almeno un elemento obbligatori")
	}
	owner, device := downloadOwner(ctx)
	items := make([]downloads.ItemMeta, 0, len(req.Items))
	for _, it := range req.Items {
		if it.StreamId == "" {
			return nil, status.Error(codes.InvalidArgument, "stream_id obbligatorio per ogni elemento")
		}
		items = append(items, downloads.ItemMeta{
			StreamID: it.StreamId, MediaID: it.MediaId, ParentID: it.ParentId, Title: it.Title,
			SeriesTitle: it.SeriesTitle, Poster: it.Poster,
			SeasonNumber: it.SeasonNumber, EpisodeNumber: it.EpisodeNumber,
		})
	}
	ins, total, err := downloads.M.CreateBatch(ctx, downloads.BatchRequest{
		Owner: owner, Device: device, PluginID: req.PluginId, Items: items,
		VariantID: req.VariantId, AudioIDs: req.AudioIds, SubtitleIDs: req.SubtitleIds,
		NotBefore: req.NotBefore, UsePreferredHours: req.UsePreferredHours, UpgradeIfBetter: req.UpgradeIfBetter,
	})
	if err != nil && len(ins) == 0 {
		return nil, downloadErr(err)
	}
	scheme, host := clientProxyBase(ctx)
	resp := &gen.CreateDownloadsResponse{EstimatedTotalBytes: total}
	for _, in := range ins {
		resp.Downloads = append(resp.Downloads, downloadInfoProto(in, scheme+"://"+host))
	}
	return resp, nil
}

// AckDownloadFetched: the device has the whole file (see the proto).
func (h *MediaHandler) AckDownloadFetched(ctx context.Context, req *gen.DownloadIdRequest) (*gen.DownloadActionResponse, error) {
	if err := requireDownloads(); err != nil {
		return nil, err
	}
	owner, _ := downloadOwner(ctx)
	if err := downloads.M.Ack(owner, req.DownloadId); err != nil {
		return nil, downloadErr(err)
	}
	return &gen.DownloadActionResponse{Ok: true}, nil
}

func (h *MediaHandler) ListDownloads(ctx context.Context, _ *gen.ListDownloadsRequest) (*gen.ListDownloadsResponse, error) {
	if err := requireDownloads(); err != nil {
		return nil, err
	}
	owner, _ := downloadOwner(ctx)
	scheme, host := clientProxyBase(ctx)
	base := scheme + "://" + host
	quota, used, avail := downloads.M.Usage()
	resp := &gen.ListDownloadsResponse{ServerFreeBytes: avail, QuotaBytes: quota, UsedBytes: used}
	for _, in := range downloads.M.List(owner) {
		resp.Downloads = append(resp.Downloads, downloadInfoProto(in, base))
	}
	return resp, nil
}

func (h *MediaHandler) CancelDownload(ctx context.Context, req *gen.DownloadIdRequest) (*gen.DownloadActionResponse, error) {
	if err := requireDownloads(); err != nil {
		return nil, err
	}
	owner, _ := downloadOwner(ctx)
	if err := downloads.M.Cancel(owner, req.DownloadId); err != nil {
		return nil, downloadErr(err)
	}
	return &gen.DownloadActionResponse{Ok: true}, nil
}

func (h *MediaHandler) DeleteDownload(ctx context.Context, req *gen.DownloadIdRequest) (*gen.DownloadActionResponse, error) {
	if err := requireDownloads(); err != nil {
		return nil, err
	}
	owner, _ := downloadOwner(ctx)
	if err := downloads.M.Delete(owner, req.DownloadId); err != nil {
		return nil, downloadErr(err)
	}
	return &gen.DownloadActionResponse{Ok: true}, nil
}

func downloadInfoProto(in *downloads.Info, base string) *gen.DownloadInfo {
	out := &gen.DownloadInfo{
		DownloadId: in.ID, PluginId: in.PluginID, StreamId: in.StreamID, MediaId: in.MediaID,
		ParentId: in.ParentID, Title: in.Title, SeriesTitle: in.SeriesTitle, Poster: in.Poster,
		SeasonNumber: in.Season, EpisodeNumber: in.Episode, Status: in.Status, Error: in.Error,
		Progress: in.Progress, EstimatedBytes: in.EstimatedBytes, FileBytes: in.FileBytes,
		QualityLabel: in.QualityLabel, AudioLanguages: in.AudioLangs, SubtitleLanguages: in.SubLangs,
		CreatedAt: in.CreatedAt, ScheduledFor: in.ScheduledFor, CompletedAt: in.CompletedAt,
		ExpiresAt: in.ExpiresAt, DurationSec: in.DurationSec, Container: "mkv",
		UpgradePending: in.UpgradePending, Shared: in.Shared, Fetched: in.Fetched,
	}
	if in.FilePath != "" {
		out.FileUrl = base + in.FilePath
	}
	return out
}
