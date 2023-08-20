package mysqlparser

type fieldType byte

const (
	fieldTypeDecimal fieldType = iota
	fieldTypeTiny
	fieldTypeShort
	fieldTypeLong
	fieldTypeFloat
	fieldTypeDouble
	fieldTypeNULL
	fieldTypeTimestamp
	fieldTypeLongLong
	fieldTypeInt24
	fieldTypeDate
	fieldTypeTime
	fieldTypeDateTime
	fieldTypeYear
	fieldTypeNewDate
	fieldTypeVarChar
	fieldTypeBit
)
const (
	fieldTypeJSON fieldType = iota + 0xf5
	fieldTypeNewDecimal
	fieldTypeEnum
	fieldTypeSet
	fieldTypeTinyBLOB
	fieldTypeMediumBLOB
	fieldTypeLongBLOB
	fieldTypeBLOB
	fieldTypeVarString
	fieldTypeString
	fieldTypeGeometry
)

var fieldTypeNames = map[fieldType]string{
	fieldTypeDecimal:    "fieldTypeDecimal",
	fieldTypeTiny:       "fieldTypeTiny",
	fieldTypeShort:      "fieldTypeShort",
	fieldTypeLong:       "fieldTypeLong",
	fieldTypeFloat:      "fieldTypeFloat",
	fieldTypeDouble:     "fieldTypeDouble",
	fieldTypeNULL:       "fieldTypeNULL",
	fieldTypeTimestamp:  "fieldTypeTimestamp",
	fieldTypeLongLong:   "fieldTypeLongLong",
	fieldTypeInt24:      "fieldTypeInt24",
	fieldTypeDate:       "fieldTypeDate",
	fieldTypeTime:       "fieldTypeTime",
	fieldTypeDateTime:   "fieldTypeDateTime",
	fieldTypeYear:       "fieldTypeYear",
	fieldTypeNewDate:    "fieldTypeNewDate",
	fieldTypeVarChar:    "fieldTypeVarChar",
	fieldTypeBit:        "fieldTypeBit",
	fieldTypeJSON:       "fieldTypeJSON",
	fieldTypeNewDecimal: "fieldTypeNewDecimal",
	fieldTypeEnum:       "fieldTypeEnum",
	fieldTypeSet:        "fieldTypeSet",
	fieldTypeTinyBLOB:   "fieldTypeTinyBLOB",
	fieldTypeMediumBLOB: "fieldTypeMediumBLOB",
	fieldTypeLongBLOB:   "fieldTypeLongBLOB",
	fieldTypeBLOB:       "fieldTypeBLOB",
	fieldTypeVarString:  "fieldTypeVarString",
	fieldTypeString:     "fieldTypeString",
	fieldTypeGeometry:   "fieldTypeGeometry",
}
