package models

// SandboxManifest represents the manifest stored in MongoDB for a sandbox reference.
// It contains file hashes for all mock files associated with a sandbox ref.
type SandboxManifest struct {
	// Ref is the sandbox reference in the format <company>/<service>:<tag>.
	Ref string `json:"ref" yaml:"ref" bson:"ref"`
	// Company is the company/org name extracted from the ref.
	Company string `json:"company" yaml:"company" bson:"company"`
	// AppName is the service/app name extracted from the ref.
	AppName string `json:"appName" yaml:"appName" bson:"app_name"`
	// Tag is the version tag extracted from the ref.
	Tag string `json:"tag" yaml:"tag" bson:"tag"`
	// Files contains the file path to hash mapping.
	Files []SandboxFileHash `json:"files" yaml:"files" bson:"files"`
	// CreatedAt is the Unix timestamp of creation.
	CreatedAt int64 `json:"createdAt" yaml:"createdAt" bson:"created_at"`
	// UpdatedAt is the Unix timestamp of last update.
	UpdatedAt int64 `json:"updatedAt" yaml:"updatedAt" bson:"updated_at"`
}

// SandboxFileHash represents a file and its content hash in the manifest.
type SandboxFileHash struct {
	// Path is the relative file path (preserving directory structure).
	Path string `json:"path" yaml:"path" bson:"path"`
	// Hash is the SHA-256 hash of the file content.
	Hash string `json:"hash" yaml:"hash" bson:"hash"`
}

// SandboxSyncResult represents the result of a sandbox sync operation.
type SandboxSyncResult struct {
	// NeedsDownload is true if local files don't match the manifest.
	NeedsDownload bool
	// MismatchedFiles lists files whose hashes don't match.
	MismatchedFiles []string
	// MissingFiles lists files that are missing locally.
	MissingFiles []string
	// MatchedFiles lists files that match the manifest.
	MatchedFiles []string
}
