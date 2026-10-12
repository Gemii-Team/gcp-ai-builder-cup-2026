package main

import (
	"context"
	"maps"
	"slices"
)

// Command is one kiwi subcommand. args excludes the subcommand name.
type Command func(ctx context.Context, args []string) error

var commands = map[string]Command{}

// register adds a subcommand. Each subcommand calls it from an init function in
// its own file, so adding one never means editing main.go.
func register(name string, c Command) {
	if _, taken := commands[name]; taken {
		panic("kiwi: command " + name + " registered twice")
	}
	commands[name] = c
}

func commandNames() []string {
	return slices.Sorted(maps.Keys(commands))
}
