package acme

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestZapLoggerForwardsToSlog(t *testing.T) {
	var buf bytes.Buffer
	sl := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	zl := zapLogger(sl).Named("certmagic").With(zap.String("component", "acme"))

	zl.Debug("hidden")
	zl.Info("obtained", zap.String("domain", "example.com"), zap.Int("tries", 2))
	zl.Warn("slow")
	zl.Error("failed", zap.Error(errTest))

	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Fatalf("debug should be filtered: %s", out)
	}
	for _, want := range []string{
		"level=INFO", `msg=obtained`, "domain=example.com", "tries=2", "component=acme", "logger=certmagic",
		"level=WARN", "level=ERROR", "error=boom",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestZapLoggerNilUsesDefault(t *testing.T) {
	if zapLogger(nil) == nil {
		t.Fatal("nil logger should still produce a zap logger")
	}
	c := &slogCore{log: discardLogger()}
	if err := c.Sync(); err != nil {
		t.Fatal(err)
	}
	if c.Check(zapcore.Entry{Level: zapcore.FatalLevel}, nil) != nil {
		// discard logger at default level still enables Error+; FatalLevel maps to Error
		t.Log("entry enabled, as expected")
	}
}

func TestSlogLevelMapping(t *testing.T) {
	cases := map[zapcore.Level]slog.Level{
		zapcore.DebugLevel:  slog.LevelDebug,
		zapcore.InfoLevel:   slog.LevelInfo,
		zapcore.WarnLevel:   slog.LevelWarn,
		zapcore.ErrorLevel:  slog.LevelError,
		zapcore.DPanicLevel: slog.LevelError,
		zapcore.PanicLevel:  slog.LevelError,
		zapcore.FatalLevel:  slog.LevelError,
	}
	for z, want := range cases {
		if got := slogLevel(z); got != want {
			t.Errorf("%v -> %v, want %v", z, got, want)
		}
	}
}

type testErr struct{}

func (testErr) Error() string { return "boom" }

var errTest error = testErr{}
