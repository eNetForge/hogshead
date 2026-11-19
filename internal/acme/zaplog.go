package acme

import (
	"context"
	"log/slog"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// zapLogger returns a *zap.Logger that forwards every entry to log.
// certmagic only speaks zap; the rest of hogshead uses slog.
func zapLogger(log *slog.Logger) *zap.Logger {
	if log == nil {
		log = slog.Default()
	}
	return zap.New(&slogCore{log: log})
}

// slogCore is a zapcore.Core backed by an *slog.Logger.
type slogCore struct {
	log    *slog.Logger
	fields []zapcore.Field
}

func slogLevel(l zapcore.Level) slog.Level {
	switch {
	case l <= zapcore.DebugLevel:
		return slog.LevelDebug
	case l == zapcore.InfoLevel:
		return slog.LevelInfo
	case l == zapcore.WarnLevel:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

func (c *slogCore) Enabled(l zapcore.Level) bool {
	return c.log.Enabled(context.Background(), slogLevel(l))
}

func (c *slogCore) With(fields []zapcore.Field) zapcore.Core {
	merged := make([]zapcore.Field, 0, len(c.fields)+len(fields))
	merged = append(merged, c.fields...)
	merged = append(merged, fields...)
	return &slogCore{log: c.log, fields: merged}
}

func (c *slogCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c *slogCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range c.fields {
		f.AddTo(enc)
	}
	for _, f := range fields {
		f.AddTo(enc)
	}
	attrs := make([]any, 0, 2*len(enc.Fields)+2)
	if e.LoggerName != "" {
		attrs = append(attrs, "logger", e.LoggerName)
	}
	for k, v := range enc.Fields {
		attrs = append(attrs, k, v)
	}
	c.log.Log(context.Background(), slogLevel(e.Level), e.Message, attrs...)
	return nil
}

func (c *slogCore) Sync() error { return nil }
