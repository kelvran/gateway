package router

// Deployment is one weighted routing candidate for a canonical model.
// Deliberately decoupled from dataplane.Deployment — this package must
// never import gateway/internal/gateway/dataplane, mirroring
// ratelimit.KeyConfig's existing decoupling from identity.VirtualKey.
type Deployment struct {
	Name  string
	Model string
	// Weight controls this deployment's share of routing selection among
	// deployments serving the same Model, per
	// docs/rfcs/2026-09-04-weighted-routing.md. Zero means "unset" and is
	// normalized to 1 in newModelState — never here, and never in
	// controlplane's parser, which only validates it's non-negative.
	Weight int
}
