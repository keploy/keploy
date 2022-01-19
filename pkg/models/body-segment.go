package models

type BodySegment struct {
	Normal    bool
	Missing   bool
	Type      SegmentType
	ValueType SegmentValueType
	Key       string
	Value     *BodySegment
	String    string
	Number    float64
	Bool      bool
	Array     []BodySegment
}

type SegmentValueType string

const (
	STRING SegmentValueType = "STRING"
	NUMBER SegmentValueType = "NUMBER"
	BOOL   SegmentValueType = "BOOL"
	NULL   SegmentValueType = "NULL"
	ARRAY  SegmentValueType = "ARRAY"
	OBJECT SegmentValueType = "OBJECT"
)

type SegmentType string

const (
	ROOT  SegmentType = "ROOT"
	KEY   SegmentType = "KEY"
	VALUE SegmentType = "VALUE"
)
