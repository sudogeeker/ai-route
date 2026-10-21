package logging

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const (
	LevelDebug = iota
	LevelInfo
	LevelWarn
	LevelError
)

type Logger struct {
	level  int
	logger *log.Logger
	output *asyncWriter
}

func New(level int, writer io.Writer) *Logger {
	return newLogger(level, writer, nil)
}

func newLogger(level int, writer io.Writer, closeOutput func() error) *Logger {
	output := newAsyncWriter(writer, closeOutput)
	return &Logger{level: level, logger: log.New(output, "", log.LstdFlags|log.Lmicroseconds), output: output}
}
func Open(level, path string) (*Logger, error) {
	severity := LevelError
	switch strings.ToUpper(level) {
	case "DEBUG":
		severity = LevelDebug
	case "INFO":
		severity = LevelInfo
	case "WARN":
		severity = LevelWarn
	}
	var writer io.Writer = os.Stdout
	var closeOutput func() error
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, err
		}
		writer = io.MultiWriter(os.Stdout, file)
		closeOutput = file.Close
	}
	return newLogger(severity, writer, closeOutput), nil
}
func (l *Logger) Close() error {
	return l.output.Close()
}
func (l *Logger) Enabled(level int) bool { return l.level <= level }

func (l *Logger) Debugf(f string, v ...interface{}) {
	if l.level <= LevelDebug {
		l.logger.Printf("[DEBG] "+f, v...)
	}
}

func (l *Logger) Infof(f string, v ...interface{}) {
	if l.level <= LevelInfo {
		l.logger.Printf("[INFO] "+f, v...)
	}
}

func (l *Logger) Warnf(f string, v ...interface{}) {
	if l.level <= LevelWarn {
		l.logger.Printf("[WARN] "+f, v...)
	}
}

func (l *Logger) Errorf(f string, v ...interface{}) {
	if l.level <= LevelError {
		l.logger.Printf("[ERRO] "+f, v...)
	}
}
