package delivery

// EnvironmentReader holds the environment fit that one run built in memory and hands it to the query handler
// UnmetRequirements (ST-DLV-RM01-RD). The handler reads nothing else, and nothing is kept between runs.
type EnvironmentReader struct {
	fit EnvironmentFit
}

// NewEnvironmentReader gives a reader of the environment fit.
func NewEnvironmentReader(fit EnvironmentFit) *EnvironmentReader {
	return &EnvironmentReader{fit: fit}
}

// Fit gives the environment fit.
func (r *EnvironmentReader) Fit() EnvironmentFit {
	return r.fit
}
