package cli

import (
	"flag"
	"fmt"
	"strings"
)

// splitArgs separates flags from positional arguments so that global flags may
// appear anywhere on the line, including after positional arguments:
//
//	mcp-cli call github search_issues query=bug -raw -p
//
// Go's flag package stops parsing at the first non-flag argument, which would
// otherwise hand "-raw" to the tool as an argument. A literal "--" ends flag
// parsing, so a tool argument that really does start with a dash can still be
// passed through.
func splitArgs(fs *flag.FlagSet, args []string) (flags, positional []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			return flags, positional, nil
		}
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}

		name := strings.TrimLeft(arg, "-")
		value, hasValue := "", false
		if eq := strings.Index(name, "="); eq >= 0 {
			name, value, hasValue = name[:eq], name[eq+1:], true
		}

		f := fs.Lookup(name)
		if f == nil {
			// Let the flag package produce the error and its usage text.
			return nil, nil, fmt.Errorf("flag provided but not defined: -%s", name)
		}

		flags = append(flags, arg)
		if hasValue || isBoolFlag(f) {
			continue
		}
		// A non-boolean flag takes the next argument as its value.
		if i+1 >= len(args) {
			return nil, nil, fmt.Errorf("flag needs an argument: -%s", name)
		}
		i++
		flags = append(flags, args[i])
		_ = value
	}
	return flags, positional, nil
}

// isBoolFlag reports whether f is a boolean flag, which takes no separate value.
func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}
