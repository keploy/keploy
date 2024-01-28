package structs

// TrafficDirectionEnum is a GO-equivalent for the following enum.
//
//	enum traffic_direction_t {
//		kEgress,
//		kIngress,
//	};.
type TrafficDirectionEnum int32

const (
	EgressTraffic  TrafficDirectionEnum = 0
	IngressTraffic TrafficDirectionEnum = 1
)

func (t TrafficDirectionEnum) String() string {
	names := [...]string{
		"EgressTraffic",
		"IngressTraffic",
	}

	switch t {
	case EgressTraffic:
		return names[0]
	case IngressTraffic:
		return names[1]
	default:
		return "Invalid TrafficDirectionEnum value"
	}
}
