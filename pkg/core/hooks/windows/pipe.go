package windows

type Pipe struct {
	Name       string
	IncomingCh map[IncomingChKey]chan Buffer
	OutgoingCh chan Buffer
}

func (p *Pipe) NewPipe(name string) *Pipe {
	return &Pipe{
		Name:       name,
		IncomingCh: make(map[IncomingChKey]chan Buffer),
		OutgoingCh: make(chan Buffer),
	}
}

const (
	IPCBufSize = 4096
)

func (p *Pipe) Start() error {
	npipe.
}
