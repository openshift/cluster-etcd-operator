package health

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	StdErrLogOutput = "stderr"
	StdOutLogOutput = "stdout"
)

type LogLine struct {
	Level           string `json:"level"`
	UnixTS          string `json:"ts"`
	Caller          string `json:"caller,omitempty"`
	Message         string `json:"msg"`
	Pod             string `json:"pod,omitempty"`
	DisruptionStart string `json:"start,omitempty"`
	DisruptionEnd   string `json:"end,omitempty"`
	Duration        string `json:"duration,omitempty"`
	Check           string `json:"check,omitempty"`
}

var (
	ErrLogRotationInvalidLogOutput = fmt.Errorf("--log-outputs requires a single file path when --log-rotate-config-json is defined")
)

type logRotationConfig struct {
	*lumberjack.Logger
}

// Sync implements zap.Sink
func (logRotationConfig) Sync() error { return nil }

func GetZapLogger(logLevel zapcore.Level, logOutputs []string, enableLogRotation bool, logRotateConfigJSON string) (*zap.Logger, error) {
	if enableLogRotation {
		if err := setupLogRotation(logOutputs, logRotateConfigJSON); err != nil {
			return nil, err
		}
	}
	outputPaths, errOutputPaths := make([]string, 0), make([]string, 0)
	for _, v := range logOutputs {
		switch v {
		case StdErrLogOutput:
			outputPaths = append(outputPaths, StdErrLogOutput)
			errOutputPaths = append(errOutputPaths, StdErrLogOutput)
		case StdOutLogOutput:
			outputPaths = append(outputPaths, StdOutLogOutput)
			errOutputPaths = append(errOutputPaths, StdOutLogOutput)
		default:
			// output is file path
			// append rotate scheme to logs managed by lumberjack log rotation
			var path string
			if enableLogRotation {
				// append rotate scheme to logs managed by lumberjack log rotation
				path = fmt.Sprintf("rotate:%s", v)
			} else {
				path = v
			}
			outputPaths = append(outputPaths, path)
			errOutputPaths = append(errOutputPaths, path)
		}
	}
	logConfig := zap.Config{
		Level: zap.NewAtomicLevelAt(logLevel),

		Development: false,
		Sampling: &zap.SamplingConfig{
			Initial:    100,
			Thereafter: 100,
		},

		Encoding: "json",

		// copied from "zap.NewProductionEncoderConfig" with some updates
		EncoderConfig: zapcore.EncoderConfig{
			TimeKey:       "ts",
			LevelKey:      "level",
			NameKey:       "logger",
			CallerKey:     "caller",
			MessageKey:    "msg",
			StacktraceKey: "stacktrace",
			LineEnding:    zapcore.DefaultLineEnding,
			EncodeLevel:   zapcore.LowercaseLevelEncoder,
			EncodeTime: zapcore.TimeEncoder(func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
				enc.AppendString(t.UTC().Format("2006-01-02T15:04:05.000Z"))
			}),
			EncodeDuration: zapcore.StringDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
		},

		OutputPaths:      outputPaths,
		ErrorOutputPaths: errOutputPaths,
	}
	return logConfig.Build()
}

func setupLogRotation(logOutputs []string, logRotateConfigJSON string) error {
	var logRotationConfig logRotationConfig
	outputFilePaths := 0
	for _, v := range logOutputs {
		switch v {
		case StdErrLogOutput, StdOutLogOutput:
			continue
		default:
			outputFilePaths++
		}
	}
	// log rotation requires file target
	if len(logOutputs) == 1 && outputFilePaths == 0 {
		return ErrLogRotationInvalidLogOutput
	}
	// support max 1 file target for log rotation
	if outputFilePaths > 1 {
		return ErrLogRotationInvalidLogOutput
	}

	if err := json.Unmarshal([]byte(logRotateConfigJSON), &logRotationConfig); err != nil {
		var unmarshalTypeError *json.UnmarshalTypeError
		var syntaxError *json.SyntaxError
		switch {
		case errors.As(err, &syntaxError):
			return fmt.Errorf("improperly formatted log rotation config: %w", err)
		case errors.As(err, &unmarshalTypeError):
			return fmt.Errorf("invalid log rotation config: %w", err)
		}
	}

	zap.RegisterSink("rotate", func(u *url.URL) (zap.Sink, error) {
		logRotationConfig.Filename = u.Path
		return &logRotationConfig, nil
	})
	return nil
}

func LoglevelToZap(logLevel int) zapcore.Level {
	switch logLevel {
	case 3:
		return zap.WarnLevel
	case 4, 5, 6:
		return zap.DebugLevel
	default:
		return zap.InfoLevel
	}
}
