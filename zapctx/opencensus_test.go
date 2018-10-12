package zapctx_test

import (
	"context"
	"go.opencensus.io/trace"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zapctx"
	"go.uber.org/zap/zaptest/observer"
	"testing"
)

func TestWithContextExtractor(t *testing.T) {
	core, obs := observer.New(zapcore.DebugLevel)
	l := zapctx.New(
		core,
		zapctx.WithContextExtractor(&ocExtractor{}))
	ctx := context.Background()
	ctx, span := trace.StartSpan(ctx, "TestSpan")
	l.Info(ctx, "message 1")
	span.End()
	l.Info(context.Background(), "message 2")
	logs := obs.All()
	if len(logs) != 2 {
		t.Fatalf("expected 2 log entries: %v", logs)
	}
	first := logs[0]
	if got, want := first.Message, "message 1"; got != want {
		t.Fatalf("unexpected first log message: %v", got)
	}
	if got, want := len(first.Context), 2; got != want {
		t.Fatalf("len = %d; want %d", got, want)
	}

}

type ocExtractor struct{}

func (e *ocExtractor) CountFields(ctx context.Context) int {
	if span := trace.FromContext(ctx); span != nil {
		return 2
	}
	return 0
}

func (e *ocExtractor) ExtractFields(ctx context.Context, into []zapctx.Field) []zapctx.Field {
	span := trace.FromContext(ctx)
	if span != nil {
		sc := span.SpanContext()
		into = append(into,
			zapctx.Binary("trace_id", sc.TraceID[:]),
			zapctx.Binary("span_id", sc.SpanID[:]))
	}
	return into
}
