package cmd

import (
	"io"

	"github.com/sirupsen/logrus"
)

// loggers registered by each package via RegisterLogger so global flags
// (--verbose/--debug/--log-format/--log-file) propagate everywhere.
var loggers []*logrus.Logger

// RegisterLogger records a logger for global level/format/output propagation.
func RegisterLogger(l *logrus.Logger) { loggers = append(loggers, l) }

func SetLogLevel(level logrus.Level) {
	for _, l := range loggers {
		l.SetLevel(level)
	}
}

func SetLogFormat(f logrus.Formatter) {
	for _, l := range loggers {
		l.SetFormatter(f)
	}
}

func SetLogOutput(w io.Writer) {
	for _, l := range loggers {
		l.SetOutput(w)
	}
}
