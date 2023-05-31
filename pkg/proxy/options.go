package proxy

// Option provides a means to initiate the proxy based on user input.
type Option struct {
	// StartingPort is the port number from which the proxy will initiate on unoccupied ports.
	StartingPort uint32 
	// Count is the number of proxies to be initiated
	Count int
}