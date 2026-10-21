package auth

// ModelAccess is a read-only credential/model policy for catalog enumeration.
// Nil sets are unrestricted; non-nil empty sets deny all access.
// It deliberately excludes transient scheduling and concurrency state.
type ModelAccess struct {
	authIDs      map[string]struct{}
	modelIDs     map[string]struct{}
	modelAuthIDs map[string]map[string]struct{}
}

// NewModelAccess normalizes catalog restrictions using the dispatch filters.
// Model-specific credential sets are intersected with the base credential set.
func NewModelAccess(authIDs, modelIDs []string, modelAuthIDs map[string][]string) *ModelAccess {
	opts := Options{Metadata: make(map[string]any)}
	if authIDs != nil {
		opts.Metadata[AllowedAuthIDsMetadataKey] = authIDs
	}
	if modelIDs != nil {
		opts.Metadata[AllowedModelIDsMetadataKey] = modelIDs
	}
	access := &ModelAccess{
		authIDs:      allowedAuthIDsFromOptions(opts),
		modelIDs:     allowedModelIDsFromOptions(opts),
		modelAuthIDs: make(map[string]map[string]struct{}, len(modelAuthIDs)),
	}
	for modelID, ids := range modelAuthIDs {
		if ids == nil {
			continue
		}
		modelKey := normalizedAllowedModelID(modelID)
		allowed := allowedAuthIDsFromOptions(Options{
			Metadata: map[string]any{AllowedAuthIDsMetadataKey: ids},
		})
		if previous, exists := access.modelAuthIDs[modelKey]; exists {
			for authID := range allowed {
				previous[authID] = struct{}{}
			}
		} else {
			access.modelAuthIDs[modelKey] = allowed
		}
	}
	return access
}

// Allows reports whether a credential may expose this public model ID.
// A nil policy preserves the unscoped catalog used outside database-backed Home.
func (a *ModelAccess) Allows(authID, modelID string) bool {
	if a == nil {
		return true
	}
	return authAllowedByID(authID, a.authIDs) &&
		modelAllowedByID(modelID, a.modelIDs) &&
		authAllowedByID(authID, a.modelAuthIDs[normalizedAllowedModelID(modelID)])
}
