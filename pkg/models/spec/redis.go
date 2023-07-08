package spec

type RedisSpec struct {
	Metadata map[string]string
	Command  string
	Response string
}

func (r *RedisSpec) Encode(rs *RedisSpec) error {

	return nil
}
