package spec

type RedisInteraction struct {
	Request  []string
	Response string
}

type RedisSpec struct {
	Metadata     map[string]string
	Interactions []RedisInteraction
}
