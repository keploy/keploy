// Package log provides utility functions for logging.
package log

import (
	"bytes"

	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

type color struct {
	*zapcore.EncoderConfig
	zapcore.Encoder
}

func NewColor(cfg zapcore.EncoderConfig, enableColor bool) (enc zapcore.Encoder) {
	if enableColor {
		return color{
			EncoderConfig: &cfg,
			Encoder:       zapcore.NewConsoleEncoder(cfg),
		}
	}
	// fmt.Println("Color is disabled")
	return zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
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
