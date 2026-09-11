package managers

type DomainManager struct{}

var Domain = &DomainManager{}

// UpdateActiveDomains was the refresh hook for gRPC provider plugins that
// declared has_dynamic_domain and recomputed their working domain in Setup().
// Lua plugins rotate their own mirrors internally, so this is now a no-op kept
// only so the scheduler's "domains" job has something to call.
func (m *DomainManager) UpdateActiveDomains() error { return nil }
