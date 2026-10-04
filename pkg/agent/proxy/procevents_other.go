//go:build !linux

package proxy

import "context"

func (p *Proxy) watchStarts(context.Context) {}
