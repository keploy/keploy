package utils

import "io"

// ExecOption is an optional setting of ExecuteCommand.
type ExecOption func(*execOptions)

type execOptions struct {
	outputTee func() io.Writer
}

// WithOutputTee has ExecuteCommand copy what the command prints, cut as
// keploy's console gets it, to a writer of its own for each of the command's
// output streams, from tee (SetChildConsoleTee). The command then always
// prints through keploy's pipes, terminal or not. keploy's own compose stack
// run as a child (`docker compose -f - up`) uses it to keep each container's
// recent output in memory, as the compose library's consumer does.
func WithOutputTee(tee func() io.Writer) ExecOption {
	return func(o *execOptions) { o.outputTee = tee }
}

// WithOutputAlso has the child's output, as WithOutputTee has it, written to
// w as well, beside the tee an earlier option set: a `docker compose up` keeps
// what it said last, for the refusal of a port it could not publish.
func WithOutputAlso(w io.Writer) ExecOption {
	return func(o *execOptions) {
		prev := o.outputTee
		if prev == nil {
			o.outputTee = func() io.Writer { return w }
			return
		}
		o.outputTee = func() io.Writer { return io.MultiWriter(prev(), w) }
	}
}

func execOptionsOf(opts []ExecOption) execOptions {
	var o execOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}
