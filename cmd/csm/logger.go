package main

import (
	"fmt"
	"io"
)

type Logger struct {
	out io.Writer
	err io.Writer
}

func (l *Logger) Info(format string, args ...any) {
	fmt.Fprintf(l.out, format+"\n", args...)
}

func (l *Logger) Blank() {
	fmt.Fprintln(l.out)
}

func (l *Logger) Success(format string, args ...any) {
	fmt.Fprintf(l.out, "✓ "+format+"\n", args...)
}

func (l *Logger) Row(label, value string) {
	fmt.Fprintf(l.out, "%-44s %s\n", label, value)
}

func (l *Logger) Check(label string, ok bool) {
	mark := "✓"

	if !ok {
		mark = "✗"
	}

	l.Row(label, mark)
}

func (l *Logger) Prompt(text string) {
	fmt.Fprint(l.out, text)
}

func (l *Logger) Error(format string, args ...any) {
	fmt.Fprintf(l.err, format+"\n", args...)
}
