package engine

import (
	"errors"
	"fmt"
)

// Typed plugin-call failures, so callers (the Pileus gRPC layer) can tell the
// user something more useful than a generic internal error.
var (
	// ErrPluginBusy: no free Lua state within the wait budget — transient.
	ErrPluginBusy = errors.New("plugin occupato")
	// ErrPluginTimeout: the entrypoint exceeded its time budget.
	ErrPluginTimeout = errors.New("timeout")
	// ErrPluginNotLoaded: no plugin with that id is loaded.
	ErrPluginNotLoaded = errors.New("plugin non caricato")
)

// PluginError is a failure the plugin reported itself (`return nil, "msg"`).
// Msg is written for the user (e.g. "Doppiaggio italiano non disponibile").
type PluginError struct {
	PluginID, Fn, Msg string
}

func (e *PluginError) Error() string {
	return fmt.Sprintf("plugin %q %s: %s", e.PluginID, e.Fn, e.Msg)
}
