package log

import (
	"bytes"

	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

type color struct {
	*zapcore.EncoderConfig
	zapcore.Encoder
}

func NewColor(cfg zapcore.EncoderConfig) (enc zapcore.Encoder) {
	return color{
		EncoderConfig: &cfg,
		// Using the default ConsoleEncoder can avoid rewriting interfaces such as ObjectEncoder
		Encoder: zapcore.NewConsoleEncoder(cfg),
	}
}

// EncodeEntry overrides ConsoleEncoder's EncodeEntry
func (c color) EncodeEntry(ent zapcore.Entry, fields []zapcore.Field) (buf *buffer.Buffer, err error) {
	buff, err := c.Encoder.EncodeEntry(ent, fields) // Utilize the existing implementation of zap
	if err != nil {
		return nil, err
	}

	bytesArr := bytes.Replace(buff.Bytes(), []byte("\\u001b"), []byte("\u001b"), -1)
	buff.Reset()
	buff.AppendString(string(bytesArr))
	return buff, err
}

// Clone overrides ConsoleEncoder's Clone
func (c color) Clone() zapcore.Encoder {
	clone := c.Encoder.Clone()
	return color{
		EncoderConfig: c.EncoderConfig,
		Encoder:       clone,
	}
}
