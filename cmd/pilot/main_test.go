package main

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestEveryCommandDeclaresJSONBehaviour is the guard that keeps `--json`
// coverage from rotting.
//
// `--json` is a global flag, so it is accepted by every command whether or not
// that command does anything with it. A command that silently ignores it leaves
// a caller waiting for output that never arrives, which is worse than an error.
//
// The annotation is not documentation — this test is the reason it exists. A new
// command, or a new subcommand, fails here until somebody decides what it does
// with the flag. That is the same mechanism as proto.SchemaDigest, for the same
// reason: a list maintained by hand, with nothing checking that it was, is a
// list that is eventually wrong in production.
func TestEveryCommandDeclaresJSONBehaviour(t *testing.T) {
	valid := map[string]bool{
		jsonStructured: true, jsonNDJSON: true,
		jsonRefused: true, jsonNone: true, jsonGroup: true,
	}

	root := buildRoot(&globals{})
	var walk func(*cobra.Command)
	var checked int

	walk = func(cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			// Cobra's own help/completion commands are not ours to annotate.
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			checked++
			how := sub.Annotations[jsonAnnotation]
			switch {
			case how == "":
				t.Errorf("`%s` does not declare how it treats --json\n"+
					"wrap it in annotate() with one of: structured, ndjson, refused, none, group",
					sub.CommandPath())
			case !valid[how]:
				t.Errorf("`%s` declares --json as %q, which is not a known behaviour",
					sub.CommandPath(), how)
			case how == jsonGroup && !sub.HasSubCommands():
				t.Errorf("`%s` is annotated as a group but has no subcommands", sub.CommandPath())
			}
			walk(sub)
		}
	}
	walk(root)

	if checked == 0 {
		t.Fatal("walked no commands; the tree is not being built")
	}
}
