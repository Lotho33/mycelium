package pileus

import (
	"context"
	"errors"
	"log"
	"strings"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ctxKeyProfileSession carries the x-profile-session value (a one-off
// profile session token) set by authenticate().
type ctxKeyProfileSession struct{}

func profileSessionFromCtx(ctx context.Context) string {
	t, _ := ctx.Value(ctxKeyProfileSession{}).(string)
	return t
}

// errProfileLocked is what every call on a PIN-protected profile gets when
// this device has neither trust nor a session for it.
var errProfileLocked = status.Error(codes.PermissionDenied, "profilo protetto da PIN: inserisci il PIN per usarlo")

// requireProfileAccess is the check for AuthService calls acting on a
// profile named in the request (the interceptor covers x-profile-id for
// every other service).
func requireProfileAccess(ctx context.Context, deviceID, profileID string) error {
	ok, err := profileAccessible(deviceID, profileID, profileSessionFromCtx(ctx))
	if err != nil {
		return status.Error(codes.Internal, "db error: "+err.Error())
	}
	if !ok {
		return errProfileLocked
	}
	return nil
}

// decorateProfile fills the per-caller PIN flags of a ProfileResponse.
func decorateProfile(ctx context.Context, deviceID string, p *gen.ProfileResponse) {
	protected, err := profileProtected(p.ProfileId)
	if err != nil || !protected {
		p.Unlocked = err == nil
		return
	}
	p.PinProtected = true
	p.DeviceTrusted = deviceTrustedCached(deviceID, p.ProfileId)
	p.Unlocked = p.DeviceTrusted || profileSessions.valid(profileSessionFromCtx(ctx), deviceID, p.ProfileId)
}

// UnlockProfile checks a profile's PIN. remember_device makes the device
// trusted for good; otherwise a one-off session token is returned.
func (h *AuthHandler) UnlockProfile(ctx context.Context, req *gen.UnlockProfileRequest) (*gen.UnlockProfileResponse, error) {
	deviceID, err := h.deviceFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if req.ProfileId == "" {
		return nil, status.Error(codes.InvalidArgument, "profile_id required")
	}
	if !profileExists(req.ProfileId) {
		return nil, status.Error(codes.NotFound, "unknown profile")
	}
	protected, err := profileProtected(req.ProfileId)
	if err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}
	if !protected {
		return &gen.UnlockProfileResponse{Ok: true}, nil
	}

	wait, err := verifyProfilePin(deviceID, req.ProfileId, req.Pin)
	if wait > 0 {
		return nil, status.Error(codes.ResourceExhausted, lockoutMessage(wait))
	}
	if errors.Is(err, errPinWrong) {
		log.Printf("[pileus/pin] PIN errato per profilo %s da device %s", req.ProfileId, deviceID)
		// PermissionDenied, not Unauthenticated: clients treat the latter as an
		// expired device session.
		return nil, status.Error(codes.PermissionDenied, "PIN errato")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "pin check: "+err.Error())
	}

	if req.RememberDevice {
		if err := trustDevice(deviceID, req.ProfileId); err != nil {
			return nil, status.Error(codes.Internal, "db error: "+err.Error())
		}
		log.Printf("[pileus/pin] device %s ora fidato per il profilo %s", deviceID, req.ProfileId)
		return &gen.UnlockProfileResponse{Ok: true, DeviceTrusted: true}, nil
	}
	token, exp, err := profileSessions.create(deviceID, req.ProfileId)
	if err != nil {
		return nil, status.Error(codes.Internal, "session: "+err.Error())
	}
	return &gen.UnlockProfileResponse{
		Ok:               true,
		SessionToken:     token,
		SessionExpiresAt: exp.Unix(),
		DeviceTrusted:    deviceTrustedCached(deviceID, req.ProfileId),
	}, nil
}

// LockProfile ends this device's one-off sessions on the profile and, with
// forget_device, its trust. It only removes access, so no PIN is needed.
func (h *AuthHandler) LockProfile(ctx context.Context, req *gen.LockProfileRequest) (*gen.LockProfileResponse, error) {
	deviceID, err := h.deviceFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if req.ProfileId == "" {
		return nil, status.Error(codes.InvalidArgument, "profile_id required")
	}
	profileSessions.drop(req.ProfileId, deviceID)
	if req.ForgetDevice {
		if err := untrustDevice(deviceID, req.ProfileId); err != nil {
			return nil, status.Error(codes.Internal, "db error: "+err.Error())
		}
	}
	return &gen.LockProfileResponse{Ok: true}, nil
}

// SetProfilePin sets, changes or removes a profile's PIN, only from inside
// that profile (x-profile-id equal to profile_id); for others it's the
// dashboard's job (SetProfilePinAdmin). With a PIN set, current_pin must
// match (same limiter as unlock). Any change revokes every trust and
// session of the profile. An unprotected profile has no secret, so this
// check stops misuse from the normal app, not a modified client.
func (h *AuthHandler) SetProfilePin(ctx context.Context, req *gen.SetProfilePinRequest) (*gen.SetProfilePinResponse, error) {
	deviceID, err := h.deviceFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if req.ProfileId == "" {
		return nil, status.Error(codes.InvalidArgument, "profile_id required")
	}
	if !profileExists(req.ProfileId) {
		return nil, status.Error(codes.NotFound, "unknown profile")
	}
	if profileIDFromCtx(ctx) != req.ProfileId {
		return nil, status.Error(codes.PermissionDenied, "il PIN di un profilo si imposta solo da quel profilo o dalla dashboard")
	}
	newPin := strings.TrimSpace(req.NewPin)
	if newPin != "" && !validPinFormat(newPin) {
		return nil, status.Error(codes.InvalidArgument, errPinFormat.Error())
	}
	protected, err := profileProtected(req.ProfileId)
	if err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}
	if protected {
		wait, err := verifyProfilePin(deviceID, req.ProfileId, req.CurrentPin)
		if wait > 0 {
			return nil, status.Error(codes.ResourceExhausted, lockoutMessage(wait))
		}
		if errors.Is(err, errPinWrong) {
			return nil, status.Error(codes.PermissionDenied, "PIN attuale errato")
		}
		if err != nil {
			return nil, status.Error(codes.Internal, "pin check: "+err.Error())
		}
	}
	if err := replaceProfilePin(req.ProfileId, newPin); err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}
	log.Printf("[pileus/pin] PIN del profilo %s %s da device %s (fiducie e sessioni revocate)",
		req.ProfileId, map[bool]string{true: "impostato", false: "rimosso"}[newPin != ""], deviceID)
	return &gen.SetProfilePinResponse{Ok: true, PinProtected: newPin != ""}, nil
}
