//go:build noremote

package remote

import (
	"context"
	"errors"
)

// Built with -tags noremote: no tailscale.com in the binary (~18 MB less,
// ~9 MB less RSS). The dashboard card reports the feature as unavailable.

// Available is false: this build has no remote access.
const Available = false

var errUnavailable = errors.New("accesso remoto non incluso in questa build (-tags noremote)")

type Manager struct{ cfg Config }

func New(cfg Config) *Manager { return &Manager{cfg: cfg} }

func (m *Manager) Status() Status {
	return Status{State: StateDisabled, Hostname: m.cfg.Hostname, ControlURL: m.cfg.ControlURL, Err: errUnavailable.Error()}
}
func (m *Manager) Running() bool                 { return false }
func (m *Manager) IsTailnetHost(string) bool     { return false }
func (m *Manager) Start(Options)                 {}
func (m *Manager) Stop()                         {}
func (m *Manager) Logout(context.Context) error  { return errUnavailable }
func (m *Manager) Relogin(context.Context) error { return errUnavailable }
