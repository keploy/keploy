package proxy

// TODO: what is the need of this? currently it is not being used anywhere.

// Option provides a means to initiate the proxy based on user input.
type Option struct {
	Port          uint32
	DNSPort       uint32
	MongoPassword string
}
