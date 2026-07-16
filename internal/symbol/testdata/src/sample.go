package sample

import (
	"fmt"
	"strings"
)

// Widget is a UI element.
// It carries a name.
type Widget struct {
	Name string
}

// Render draws the widget and returns its label.
func (w Widget) Render() string {
	return decorate(w.Name)
}

// decorate wraps s in brackets.
func decorate(s string) string {
	return fmt.Sprintf("[%s]", strings.ToUpper(s))
}

// Build makes a Widget.
func Build() *Widget { return &Widget{} }

// MaxWidgets caps the pool.
const MaxWidgets = 10

var defaultWidget = Widget{}
