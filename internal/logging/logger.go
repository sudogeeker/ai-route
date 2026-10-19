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
	file   *os.File
}

func New(level int, writer io.Writer) *Logger {
	return &Logger{level: level, logger: log.New(writer, "", log.LstdFlags|log.Lmicroseconds)}
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
	logger := New(severity, os.Stdout)
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, err
		}
		logger.file = file
		logger.logger.SetOutput(io.MultiWriter(os.Stdout, file))
	}
	return logger, nil
}
func (l *Logger) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
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
