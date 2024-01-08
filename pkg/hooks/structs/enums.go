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
