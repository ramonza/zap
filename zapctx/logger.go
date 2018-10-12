// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package zapctx

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"runtime"
	"strings"
	"time"

	"go.uber.org/zap/zapcore"
)

type ContextExtractor interface {
	CountFields(ctx context.Context) int
	ExtractFields(ctx context.Context, into []Field) []Field
}

type ContextExtractorFunc func(context.Context, []Field) []Field

func (f ContextExtractorFunc) CountFields(ctx context.Context) int {
	return 8
}

func (f ContextExtractorFunc) ExtractFields(ctx context.Context, into []Field) []Field {
	return f(ctx, into)
}

// A Logger provides fast, leveled, structured logging. All methods are safe
// for concurrent use.
//
// The Logger is designed for contexts in which every microsecond and every
// allocation matters, so its API intentionally favors performance and type
// safety over brevity. For most applications, the SugaredLogger strikes a
// better balance between performance and ergonomics.
type Logger struct {
	core zapcore.Core

	development bool
	name        string
	errorOutput zapcore.WriteSyncer

	addCaller bool
	addStack  zapcore.LevelEnabler

	callerSkip int

	extractors []ContextExtractor
}

// New constructs a new Logger from the provided zapcore.Core and Options. If
// the passed zapcore.Core is nil, it falls back to using a no-op
// implementation.
//
// This is the most flexible way to construct a Logger, but also the most
// verbose. For typical use cases, the highly-opinionated presets
// (NewProduction, NewDevelopment, and NewExample) or the Config struct are
// more convenient.
//
// For sample code, see the package-level AdvancedConfiguration example.
func New(core zapcore.Core, options ...Option) *Logger {
	if core == nil {
		return NewNop()
	}
	log := &Logger{
		core:        core,
		errorOutput: zapcore.Lock(os.Stderr),
		addStack:    zapcore.FatalLevel + 1,
	}
	return log.WithOptions(options...)
}

// NewNop returns a no-op Logger. It never writes out logs or internal errors,
// and it never runs user-defined hooks.
//
// Using WithOptions to replace the Core or error output of a no-op Logger can
// re-enable logging.
func NewNop() *Logger {
	return &Logger{
		core:        zapcore.NewNopCore(),
		errorOutput: zapcore.AddSync(ioutil.Discard),
		addStack:    zapcore.FatalLevel + 1,
	}
}

// NewProduction builds a sensible production Logger that writes InfoLevel and
// above logs to standard error as JSON.
//
// It's a shortcut for NewProductionConfig().Build(...Option).
func NewProduction(options ...Option) (*Logger, error) {
	return NewProductionConfig().Build(options...)
}

// NewDevelopment builds a development Logger that writes DebugLevel and above
// logs to standard error in a human-friendly format.
//
// It's a shortcut for NewDevelopmentConfig().Build(...Option).
func NewDevelopment(options ...Option) (*Logger, error) {
	return NewDevelopmentConfig().Build(options...)
}

// NewExample builds a Logger that's designed for use in zap's testable
// examples. It writes DebugLevel and above logs to standard out as JSON, but
// omits the timestamp and calling function to keep example output
// short and deterministic.
func NewExample(options ...Option) *Logger {
	encoderCfg := zapcore.EncoderConfig{
		MessageKey:     "msg",
		LevelKey:       "level",
		NameKey:        "logger",
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
	}
	core := zapcore.NewCore(zapcore.NewJSONEncoder(encoderCfg), os.Stdout, DebugLevel)
	return New(core).WithOptions(options...)
}

// Sugar wraps the Logger to provide a more ergonomic, but slightly slower,
// API. Sugaring a Logger is quite inexpensive, so it's reasonable for a
// single application to use both Loggers and SugaredLoggers, converting
// between them on the boundaries of performance-sensitive code.
func (log *Logger) Sugar() *SugaredLogger {
	core := log.clone()
	core.callerSkip += 2
	return &SugaredLogger{core}
}

// Named adds a new path segment to the logger's name. Segments are joined by
// periods. By default, Loggers are unnamed.
func (log *Logger) Named(s string) *Logger {
	if s == "" {
		return log
	}
	l := log.clone()
	if log.name == "" {
		l.name = s
	} else {
		l.name = strings.Join([]string{l.name, s}, ".")
	}
	return l
}

// WithOptions clones the current Logger, applies the supplied Options, and
// returns the resulting Logger. It's safe to use concurrently.
func (log *Logger) WithOptions(opts ...Option) *Logger {
	c := log.clone()
	for _, opt := range opts {
		opt.apply(c)
	}
	return c
}

// With creates a child logger and adds structured context to it. Fields added
// to the child don't affect the parent, and vice versa.
func (log *Logger) With(fields ...Field) *Logger {
	if len(fields) == 0 {
		return log
	}
	l := log.clone()
	l.core = l.core.With(fields)
	return l
}

// Check returns a CheckedEntry if logging a message at the specified level
// is enabled. It's a completely optional optimization; in high-performance
// applications, Check can help avoid allocating a slice to hold fields.
func (log *Logger) Check(ctx context.Context, lvl zapcore.Level, msg string) *zapcore.CheckedEntry {
	return log.check(ctx, lvl, msg)
}

func (log *Logger) write(ctx context.Context, ce *zapcore.CheckedEntry, fields []Field) {
	n := len(fields)
	for _, v := range log.extractors {
		n += v.CountFields(ctx)
	}
	allFields := make([]Field, 0, n)
	allFields = append(allFields, fields...)
	for _, e := range log.extractors {
		allFields = e.ExtractFields(ctx, allFields)
	}
	ce.Write(allFields...)
}

// Debug logs a message at DebugLevel. The message includes any fields passed
// at the log site, as well as any fields accumulated on the logger.
func (log *Logger) Debug(ctx context.Context, msg string, fields ...Field) {
	if ce := log.check(ctx, DebugLevel, msg); ce != nil {
		log.write(ctx, ce, fields)
	}
}

// Info logs a message at InfoLevel. The message includes any fields passed
// at the log site, as well as any fields accumulated on the logger.
func (log *Logger) Info(ctx context.Context, msg string, fields ...Field) {
	if ce := log.check(ctx, InfoLevel, msg); ce != nil {
		log.write(ctx, ce, fields)
	}
}

// Warn logs a message at WarnLevel. The message includes any fields passed
// at the log site, as well as any fields accumulated on the logger.
func (log *Logger) Warn(ctx context.Context, msg string, fields ...Field) {
	if ce := log.check(ctx, WarnLevel, msg); ce != nil {
		log.write(ctx, ce, fields)
	}
}

// Error logs a message at ErrorLevel. The message includes any fields passed
// at the log site, as well as any fields accumulated on the logger.
func (log *Logger) Error(ctx context.Context, msg string, fields ...Field) {
	if ce := log.check(ctx, ErrorLevel, msg); ce != nil {
		log.write(ctx, ce, fields)
	}
}

// DPanic logs a message at DPanicLevel. The message includes any fields
// passed at the log site, as well as any fields accumulated on the logger.
//
// If the logger is in development mode, it then panics (DPanic means
// "development panic"). This is useful for catching errors that are
// recoverable, but shouldn't ever happen.
func (log *Logger) DPanic(ctx context.Context, msg string, fields ...Field) {
	if ce := log.check(ctx, DPanicLevel, msg); ce != nil {
		log.write(ctx, ce, fields)
	}
}

// Panic logs a message at PanicLevel. The message includes any fields passed
// at the log site, as well as any fields accumulated on the logger.
//
// The logger then panics, even if logging at PanicLevel is disabled.
func (log *Logger) Panic(ctx context.Context, msg string, fields ...Field) {
	if ce := log.check(ctx, PanicLevel, msg); ce != nil {
		log.write(ctx, ce, fields)
	}
}

// Fatal logs a message at FatalLevel. The message includes any fields passed
// at the log site, as well as any fields accumulated on the logger.
//
// The logger then calls os.Exit(1), even if logging at FatalLevel is
// disabled.
func (log *Logger) Fatal(ctx context.Context, msg string, fields ...Field) {
	if ce := log.check(ctx, FatalLevel, msg); ce != nil {
		log.write(ctx, ce, fields)
	}
}

// Sync calls the underlying Core's Sync method, flushing any buffered log
// entries. Applications should take care to call Sync before exiting.
func (log *Logger) Sync() error {
	return log.core.Sync()
}

// Core returns the Logger's underlying zapcore.Core.
func (log *Logger) coreFor(ctx context.Context) zapcore.Core {
	return log.core
}

func (log *Logger) clone() *Logger {
	copy := *log
	return &copy
}

func (log *Logger) check(ctx context.Context, lvl zapcore.Level, msg string) *zapcore.CheckedEntry {
	// check must always be called directly by a method in the Logger interface
	// (e.g., Check, Info, Fatal).
	const callerSkipOffset = 2

	// Create basic checked entry thru the core; this will be non-nil if the
	// log message will actually be written somewhere.
	ent := zapcore.Entry{
		LoggerName: log.name,
		Time:       time.Now(),
		Level:      lvl,
		Message:    msg,
	}
	ce := log.core.Check(ent, nil)
	willWrite := ce != nil

	// Set up any required terminal behavior.
	switch ent.Level {
	case zapcore.PanicLevel:
		ce = ce.Should(ent, zapcore.WriteThenPanic)
	case zapcore.FatalLevel:
		ce = ce.Should(ent, zapcore.WriteThenFatal)
	case zapcore.DPanicLevel:
		if log.development {
			ce = ce.Should(ent, zapcore.WriteThenPanic)
		}
	}

	// Only do further annotation if we're going to write this message; checked
	// entries that exist only for terminal behavior don't benefit from
	// annotation.
	if !willWrite {
		return ce
	}

	// Thread the error output through to the CheckedEntry.
	ce.ErrorOutput = log.errorOutput
	if log.addCaller {
		ce.Entry.Caller = zapcore.NewEntryCaller(runtime.Caller(log.callerSkip + callerSkipOffset))
		if !ce.Entry.Caller.Defined {
			fmt.Fprintf(log.errorOutput, "%v Logger.check error: failed to get caller\n", time.Now().UTC())
			log.errorOutput.Sync()
		}
	}
	if log.addStack.Enabled(ce.Entry.Level) {
		ce.Entry.Stack = Stack("").String
	}

	return ce
}

func (log *Logger) countContextFields(ctx context.Context) int {
	n := 0
	for _, e := range log.extractors {
		n += e.CountFields(ctx)
	}
	return n
}

func (log *Logger) addContextFields(ctx context.Context, fields []Field) []Field {
	for _, e := range log.extractors {
		fields = append(fields, e.ExtractFields(ctx, fields)...)
	}
	return fields
}
