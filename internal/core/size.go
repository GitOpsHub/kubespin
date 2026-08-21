package core

// ClusterSize picks a cluster's default addon footprint from the builtin
// catalog: no external profile repository, no version to pin — changing what
// a size includes means shipping a new kubespin build.
type ClusterSize string

// Supported sizes. Each has a builtin catalog entry in internal/catalog.
const (
	SizeSmall  ClusterSize = "small"
	SizeMedium ClusterSize = "medium"
	SizeLarge  ClusterSize = "large"
)

// Sizes lists every supported size, in the order help text should show them.
func Sizes() []ClusterSize { return []ClusterSize{SizeSmall, SizeMedium, SizeLarge} }

// Valid reports whether s is a supported size.
func (s ClusterSize) Valid() bool {
	switch s {
	case SizeSmall, SizeMedium, SizeLarge:
		return true
	default:
		return false
	}
}

func (s ClusterSize) String() string { return string(s) }
