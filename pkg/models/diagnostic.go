package models

type DiffKind string

const (
	DiffAdded       DiffKind = "ADDED"
	DiffRemoved     DiffKind = "REMOVED"
	DiffModified    DiffKind = "MODIFIED"
	DiffTypeChanged DiffKind = "TYPE_CHANGED"
)

type DiffCategory string

const (
	CategorySchemaChange DiffCategory = "SCHEMA_CHANGE"
	CategoryDataUpdate   DiffCategory = "DATA_UPDATE"
	CategoryDynamicNoise DiffCategory = "DYNAMIC_NOISE"
)

type DiffEntry struct {
	Path     string      `json:"path,omitempty" yaml:"path,omitempty"`
	Kind     DiffKind    `json:"kind,omitempty" yaml:"kind,omitempty"`
	Expected interface{} `json:"expected,omitempty" yaml:"expected,omitempty"`
	Actual   interface{} `json:"actual,omitempty" yaml:"actual,omitempty"`
	Category DiffCategory `json:"category,omitempty" yaml:"category,omitempty"`
}

type DiffReport struct {
	Entries    []DiffEntry  `json:"entries,omitempty" yaml:"entries,omitempty"`
	Category   DiffCategory `json:"category,omitempty" yaml:"category,omitempty"`
	Confidence int          `json:"confidence,omitempty" yaml:"confidence,omitempty"`
}
