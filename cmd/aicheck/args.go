package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The argument rules of Python's argparse, which the reference checker uses, so every checker accepts
// and rejects the same command lines: flags anywhere, a value as the next argument or after "=",
// "--" ends the options. The stdlib flag package stops at the first positional, so it cannot.

type flagKind int

const (
	boolFlag  flagKind = iota // store_true
	valueFlag                 // the last value wins
	listFlag                  // append
)

type flagSpec struct {
	kind    flagKind
	choices []string
}

var checkFlags = map[string]flagSpec{
	"spec":               {kind: valueFlag},
	"client-schema":      {kind: valueFlag},
	"user-id":            {kind: valueFlag},
	"capture-content":    {kind: boolFlag},
	"no-ai-events":       {kind: boolFlag},
	"expect-session":     {kind: boolFlag},
	"expect-joins":       {kind: boolFlag},
	"expect-legacy":      {kind: listFlag},
	"absent":             {kind: listFlag},
	"feature":            {kind: listFlag},
	"surface":            {kind: listFlag},
	"failed-interaction": {kind: listFlag},
	"mcp-server":         {kind: listFlag},
	"mcp-intent":         {kind: listFlag},
	"client-surface":     {kind: listFlag},
	"client-opt-out":     {kind: listFlag, choices: []string{"client_prompt", "client_response", "client_feedback", "server_feedback"}},
	"feedback-endpoint":  {kind: listFlag},
}

// errUsage is a command line the checker rejects: exit code 2 with the usage on stderr.
type errUsage struct{ msg string }

func (e *errUsage) Error() string { return e.msg }

func usageErr(format string, args ...any) error { return &errUsage{fmt.Sprintf(format, args...)} }

type parsedArgs struct {
	positionals []string
	bools       map[string]bool
	lists       map[string][]string
}

// last is a value flag's value; "" when not given (an empty value means the default too).
func (p *parsedArgs) last(name string) string {
	if v := p.lists[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

// argparse reads an argument that looks like a negative number as a positional.
var negativeNumber = regexp.MustCompile(`^-\p{Nd}+\n?\z|^-\p{Nd}*\.\p{Nd}+\n?\z`)

type option struct {
	name     string // "" for an unknown option
	explicit *string
}

// classify returns the option an argument names, or nil for a positional.
func classify(arg string, flags map[string]flagSpec) *option {
	if arg == "" || arg[0] != '-' {
		return nil
	}
	if name, ok := strings.CutPrefix(arg, "--"); ok {
		if _, known := flags[name]; known {
			return &option{name: name}
		}
	}
	if len(arg) == 1 {
		return nil
	}
	if prefix, value, ok := strings.Cut(arg, "="); ok {
		if name, ok := strings.CutPrefix(prefix, "--"); ok {
			if _, known := flags[name]; known {
				return &option{name: name, explicit: &value}
			}
		}
	}
	if negativeNumber.MatchString(arg) || strings.Contains(arg, " ") {
		return nil
	}
	return &option{}
}

// parseArgs parses args for positionals (each one argument) and flags, as argparse does.
func parseArgs(args []string, positionals []string, flags map[string]flagSpec) (*parsedArgs, error) {
	// The pattern has O for an option, A for an argument and - for the "--" that ends the options.
	pattern := make([]byte, len(args))
	opts := map[int]*option{}
	maxOpt := -1
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			pattern[i] = '-'
			for j := i + 1; j < len(args); j++ {
				pattern[j] = 'A'
			}
			break
		}
		if o := classify(args[i], flags); o != nil {
			pattern[i] = 'O'
			opts[i] = o
			maxOpt = i
		} else {
			pattern[i] = 'A'
		}
	}

	p := &parsedArgs{bools: map[string]bool{}, lists: map[string][]string{}}
	var extras []string
	remaining := len(positionals)

	// consumePositionals gives as many remaining positionals as match one argument each, with any
	// "--" next to it, and drops the "--".
	consumePositionals := func(start int) int {
		var counts []int
		for k := remaining; k > 0 && counts == nil; k-- {
			i, try := start, []int{}
			for range k {
				from := i
				for i < len(pattern) && pattern[i] == '-' {
					i++
				}
				if i >= len(pattern) || pattern[i] != 'A' {
					try = nil
					break
				}
				i++
				for i < len(pattern) && pattern[i] == '-' {
					i++
				}
				try = append(try, i-from)
			}
			if try != nil {
				counts = try
			}
		}
		for _, n := range counts {
			values := slices.Clone(args[start : start+n])
			if slices.Contains(pattern[start:start+n], '-') {
				i := slices.Index(values, "--")
				values = slices.Delete(values, i, i+1)
			}
			p.positionals = append(p.positionals, values[0])
			start += n
		}
		remaining -= len(counts)
		return start
	}

	consumeOptional := func(start int) (int, error) {
		o := opts[start]
		if o.name == "" {
			extras = append(extras, args[start])
			return start + 1, nil
		}
		spec := flags[o.name]
		var value string
		next := start + 1
		switch {
		case spec.kind == boolFlag && o.explicit != nil:
			return 0, usageErr("argument --%s: ignored explicit argument %s", o.name, reprStr(*o.explicit))
		case spec.kind == boolFlag:
			p.bools[o.name] = true
			return next, nil
		case o.explicit != nil:
			value = *o.explicit
		case next < len(pattern) && pattern[next] == 'A':
			value = args[next]
			next++
		default:
			return 0, usageErr("argument --%s: expected one argument", o.name)
		}
		if spec.choices != nil && !slices.Contains(spec.choices, value) {
			return 0, usageErr("argument --%s: invalid choice: %s (choose from %s)", o.name, reprStr(value), strings.Join(spec.choices, ", "))
		}
		p.lists[o.name] = append(p.lists[o.name], value)
		return next, nil
	}

	start := 0
	for start <= maxOpt {
		nextOpt := start
		for opts[nextOpt] == nil {
			nextOpt++
		}
		if start != nextOpt {
			end := consumePositionals(start)
			if end > start {
				start = end
				continue
			}
		}
		if opts[start] == nil {
			extras = append(extras, args[start:nextOpt]...)
			start = nextOpt
		}
		var err error
		if start, err = consumeOptional(start); err != nil {
			return nil, err
		}
	}
	stop := consumePositionals(start)
	if remaining > 0 {
		return nil, usageErr("the following arguments are required: %s", strings.Join(positionals[len(positionals)-remaining:], ", "))
	}
	extras = append(extras, args[stop:]...)
	if len(extras) > 0 {
		return nil, usageErr("unrecognized arguments: %s", strings.Join(extras, " "))
	}
	return p, nil
}
