package example

import (
	"fmt"
	"strings"
)

// Greeter builds greetings.
type Greeter struct {
	prefix string
}

// NewGreeter returns a Greeter.
func NewGreeter(prefix string) *Greeter {
	return &Greeter{prefix: prefix}
}

// Greet greets name.
func (g *Greeter) Greet(name string) string {
	return fmt.Sprintf("%s, %s!", g.prefix, name)
}

// Shout uppercases a greeting.
func (g *Greeter) Shout(name string) string {
	return strings.ToUpper(g.Greet(name))
}

func helper(n int) int {
	return n * 2
}
