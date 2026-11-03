package models

import (
	"maps"
	"slices"
)

// The copies Mock.DeepCopy makes of a mock's parts: each shares no map,
// slice or pointer with what it copies, and keeps a nil map or slice nil
// (but clonePayloads: see it).

// clone is r with its URL parameters, headers, form and header line
// lengths its own; nil for nil.
func (r *HTTPReq) clone() *HTTPReq {
	if r == nil {
		return nil
	}
	c := *r
	c.URLParams = maps.Clone(r.URLParams)
	c.Header = maps.Clone(r.Header)
	c.Form = cloneForm(r.Form)
	c.HeaderLineLengths = r.HeaderLineLengths.clone()
	return &c
}

// clone is r with its headers, streamed chunks and header line lengths its
// own; nil for nil.
func (r *HTTPResp) clone() *HTTPResp {
	if r == nil {
		return nil
	}
	c := *r
	c.Header = maps.Clone(r.Header)
	if r.StreamBody != nil {
		c.StreamBody = make([]HTTPStreamChunk, len(r.StreamBody))
		for i, chunk := range r.StreamBody {
			chunk.Data = slices.Clone(chunk.Data)
			c.StreamBody[i] = chunk
		}
	}
	c.HeaderLineLengths = r.HeaderLineLengths.clone()
	return &c
}

// cloneForm is form with each field's values, paths, file names and file
// contents its own.
func cloneForm(form []FormData) []FormData {
	if form == nil {
		return nil
	}
	c := make([]FormData, len(form))
	for i, f := range form {
		f.Values = slices.Clone(f.Values)
		f.Paths = slices.Clone(f.Paths)
		f.FileNames = slices.Clone(f.FileNames)
		if f.FileContents != nil {
			contents := make([][]byte, len(f.FileContents))
			for j, b := range f.FileContents {
				contents[j] = slices.Clone(b)
			}
			f.FileContents = contents
		}
		c[i] = f
	}
	return c
}

// clone is h with each header's line lengths its own.
func (h HeaderLineLengths) clone() HeaderLineLengths {
	if h == nil {
		return nil
	}
	c := make(HeaderLineLengths, len(h))
	for name, lengths := range h {
		c[name] = slices.Clone(lengths)
	}
	return c
}

// clone is h with its pseudo and ordinary headers its own.
func (h GrpcHeaders) clone() GrpcHeaders {
	return GrpcHeaders{PseudoHeaders: maps.Clone(h.PseudoHeaders), OrdinaryHeaders: maps.Clone(h.OrdinaryHeaders)}
}

// clone is r with its headers and further messages its own; nil for nil.
func (r *GrpcReq) clone() *GrpcReq {
	if r == nil {
		return nil
	}
	c := *r
	c.Headers = r.Headers.clone()
	c.AdditionalMessages = slices.Clone(r.AdditionalMessages)
	return &c
}

// clone is r with its headers, further messages and trailers its own; nil
// for nil.
func (r *GrpcResp) clone() *GrpcResp {
	if r == nil {
		return nil
	}
	c := *r
	c.Headers = r.Headers.clone()
	c.AdditionalMessages = slices.Clone(r.AdditionalMessages)
	c.Trailers = r.Trailers.clone()
	return &c
}

// clonePayloads is payloads with each payload's messages its own. A nil
// list comes back empty, not nil, as DeepCopy has always made it.
func clonePayloads(payloads []Payload) []Payload {
	c := make([]Payload, len(payloads))
	for i, p := range payloads {
		p.Message = slices.Clone(p.Message)
		c[i] = p
	}
	return c
}

// cloneBinds is binds with each mock's variables their own.
func cloneBinds(binds map[string]MockVariables) map[string]MockVariables {
	if binds == nil {
		return nil
	}
	c := make(map[string]MockVariables, len(binds))
	for name, vars := range binds {
		c[name] = vars.Clone()
	}
	return c
}
