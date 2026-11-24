package envmanifest

import (
	"regexp"
	"slices"
	"strings"
)

// envNameRe is the POSIX shell variable-name grammar.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ExportedNames returns the sorted, distinct variable names the shell
// scripts place in a child process's environment. It recognizes three forms:
//
//   - export statements, both "export NAME=value" and the bare re-export
//     "export NAME";
//   - prefix assignments on a command ("NAME=value cmd" and
//     "env NAME=value cmd"), which the command's process inherits;
//   - the same forms inside the script argument of "sh -c" or "bash -c",
//     parsed recursively.
//
// A plain assignment with no command ("NAME=value" alone, or "readonly
// NAME=value") is shell-local and is not reported, nor is a prefix
// assignment on a shell builtin ("IFS= read"). Assignment-shaped text
// inside a quoted value is data, not a binding, and a command
// substitution's exports stay in its subshell. The parser is a tokenizer,
// not a shell: it handles single and double quotes, backslash escapes,
// $(...) nesting, comments, and the ; && || | & ( ) { } newline
// separators, which is the subset the runner's emitters generate.
//
// Commands are applied in order. "export -n NAME" removes the export
// attribute, so a name un-exported before any child process runs is not
// reported ("export FOO=bar; export -n FOO"). A name is reported once it
// reaches a child process (any command that is not a shell builtin, "sh -c"
// included, or any command with a $(...) or `...` substitution) or is
// still exported when the script ends, so un-exporting it after a child
// already inherited it ("export FOO=1; cmd; export -n FOO") does not take
// it back. An "export -n" only counts when it is an unconditional
// top-level command: one inside ( ) or { }, in a pipeline or background
// job, after && or ||, or inside an if, while, until, for, select or case
// body is ignored, because it may not run or may not affect this shell.
// The parser over-reports in those cases rather than under-reports.
// "export -f" names functions, not variables, and is ignored. Each script
// starts with nothing exported, and the result is the union across
// scripts.
func ExportedNames(scripts ...string) []string {
	seen := map[string]bool{}
	for _, script := range scripts {
		collectExportedNames(script, seen, 0)
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// shellBuiltins are commands that run inside the shell itself, so a prefix
// assignment on them ("IFS= read -r line") scopes to that one builtin and
// never reaches a child process's environment.
var shellBuiltins = map[string]bool{
	":": true, "[": true, "break": true, "cd": true, "continue": true,
	"echo": true, "eval": true, "exit": true, "false": true, "printf": true,
	"read": true, "return": true, "set": true, "shift": true, "test": true,
	"true": true, "unset": true,
}

// maxShellDepth bounds sh -c recursion so a malformed input cannot loop.
const maxShellDepth = 8

// shellWord is one word of a simple command after quote removal.
// nameQuoted records that a quote opened before the word's first '=': sh
// treats 'A'=b as a command word, not an assignment.
type shellWord struct {
	text       string
	nameQuoted bool
}

// assignmentName returns NAME when w is an unquoted-name NAME=value word,
// or "" otherwise.
func (w shellWord) assignmentName() string {
	if w.nameQuoted {
		return ""
	}
	name, _, ok := strings.Cut(w.text, "=")
	if !ok || !envNameRe.MatchString(name) {
		return ""
	}
	return name
}

// exportState tracks one shell's export attributes while its script is
// walked in order.
type exportState struct {
	// seen collects the reported names, shared with nested shells.
	seen map[string]bool
	// exported holds the names that carry the export attribute right now
	// in this shell.
	exported map[string]bool
}

// spawn records that a child process starts here: it inherits every name
// currently exported.
func (s *exportState) spawn() {
	for name := range s.exported {
		s.seen[name] = true
	}
}

func collectExportedNames(script string, seen map[string]bool, depth int) {
	if depth > maxShellDepth {
		return
	}
	st := &exportState{seen: seen, exported: map[string]bool{}}
	for _, c := range shellCommands(script) {
		collectFromCommand(c.words, c.unconditional, st, depth)
	}
	// Names still exported at the end reach whatever the script runs next.
	st.spawn()
}

// collectFromCommand applies one simple command to st. unconditional
// reports that the command always runs in this shell (see shellCommand).
func collectFromCommand(words []shellWord, unconditional bool, st *exportState, depth int) {
	seen := st.seen
	// A command substitution runs a child process before the command
	// itself, even on an assignment or a builtin.
	for _, w := range words {
		if strings.Contains(w.text, "$(") || strings.Contains(w.text, "`") {
			st.spawn()
			break
		}
	}
	// Leading NAME=value words are prefix assignments when a command
	// follows them, and shell-local assignments when nothing does.
	i := 0
	for i < len(words) && words[i].assignmentName() != "" {
		i++
	}
	if i == len(words) {
		return
	}
	cmd, args := words[i].text, words[i+1:]
	if !shellBuiltins[cmd] {
		for _, w := range words[:i] {
			seen[w.assignmentName()] = true
		}
		if cmd != "export" && cmd != "readonly" {
			st.spawn()
		}
	}
	switch cmd {
	case "export":
		applyExport(args, unconditional, st)
	case "env":
		for j, a := range args {
			if strings.HasPrefix(a.text, "-") {
				continue
			}
			name := a.assignmentName()
			if name == "" {
				collectFromCommand(args[j:], unconditional, st, depth)
				return
			}
			seen[name] = true
		}
	case "exec", "command", "nohup":
		collectFromCommand(args, unconditional, st, depth)
	case "sh", "bash", "/bin/sh", "/bin/bash":
		for j, a := range args {
			if strings.HasPrefix(a.text, "-") && strings.Contains(a.text, "c") && j+1 < len(args) {
				collectExportedNames(args[j+1].text, seen, depth+1)
				return
			}
		}
	}
}

// applyExport applies the arguments of one export builtin to st. Options
// come first and end at "--" or at the first word that does not start with
// "-", as in bash. -n removes the export attribute instead of setting it
// ("export -n NAME=value" assigns but does not export), and -f makes the
// names functions, which never reach the environment as variables. -p
// only adds a listing, so its names are still exported. An un-export is
// applied only when unconditional is true; otherwise it may not run, or
// may run in a subshell, so the names stay exported.
func applyExport(args []shellWord, unconditional bool, st *exportState) {
	unexport, functions := false, false
	for len(args) > 0 && strings.HasPrefix(args[0].text, "-") {
		opt := args[0].text
		args = args[1:]
		if opt == "--" {
			break
		}
		unexport = unexport || strings.Contains(opt, "n")
		functions = functions || strings.Contains(opt, "f")
	}
	if functions {
		return
	}
	for _, a := range args {
		name := a.assignmentName()
		if name == "" && envNameRe.MatchString(a.text) {
			name = a.text
		}
		if name == "" {
			continue
		}
		if unexport {
			if unconditional {
				delete(st.exported, name)
			}
		} else {
			st.exported[name] = true
		}
	}
}

// shellCommand is one simple command. unconditional is true when the
// command always runs in the script's own shell: it is not inside ( ) or
// { }, not in a pipeline or background job, not after && or ||, and not in
// an if, while, until, for, select or case.
type shellCommand struct {
	words         []shellWord
	unconditional bool
}

// compoundOpen and compoundClose are the reserved words that start and end
// a compound command whose body may not run. compoundBody words lead a
// body command inside one.
var (
	compoundOpen  = map[string]bool{"if": true, "while": true, "until": true, "for": true, "select": true, "case": true}
	compoundClose = map[string]bool{"fi": true, "done": true, "esac": true}
	compoundBody  = map[string]bool{"then": true, "else": true, "elif": true, "do": true}
)

// condBefore reports that the command following sep may not run, or runs
// in a subshell. condAfter reports that the command ending at sep runs in
// a subshell (a pipeline member or background job).
func condBefore(sep string) bool {
	return sep == "&&" || sep == "||" || sep == "|" || sep == "|&" || sep == "&"
}

func condAfter(sep string) bool {
	return sep == "|" || sep == "|&" || sep == "&"
}

// shellCommands splits script into simple commands, each a list of words
// with quotes removed. Command substitutions are kept as opaque word text.
func shellCommands(script string) []shellCommand {
	var (
		cmds       []shellCommand
		words      []shellWord
		cur        strings.Builder
		inWord     bool
		nameQuoted bool
		sawEq      bool
		// prevSep is the separator before the current command; "" at the
		// start of the script.
		prevSep string
		// groupDepth counts open ( ) and { } groups; compoundDepth counts
		// open if/while/until/for/select/case commands.
		groupDepth, compoundDepth int
	)
	endWord := func() {
		if inWord {
			words = append(words, shellWord{text: cur.String(), nameQuoted: nameQuoted})
		}
		cur.Reset()
		inWord, nameQuoted, sawEq = false, false, false
	}
	endCmd := func(sep string) {
		endWord()
		if len(words) == 0 {
			// A newline after && || | & continues the same list.
			if sep != ";" || !condBefore(prevSep) {
				prevSep = sep
			}
			return
		}
		unconditional := groupDepth == 0 && compoundDepth == 0 &&
			!condBefore(prevSep) && !condAfter(sep)
		// Leading reserved words open or close compound commands, and a
		// command led by one is part of a compound command, so it is
		// conditional.
		for _, w := range words {
			if compoundOpen[w.text] {
				compoundDepth++
			} else if compoundClose[w.text] {
				compoundDepth = max(compoundDepth-1, 0)
			} else if !compoundBody[w.text] {
				break
			}
			unconditional = false
		}
		cmds = append(cmds, shellCommand{words: words, unconditional: unconditional})
		words, prevSep = nil, sep
	}
	quote := func() {
		inWord = true
		if !sawEq {
			nameQuoted = true
		}
	}
	for i := 0; i < len(script); i++ {
		c := script[i]
		switch {
		case c == '\'':
			quote()
			j := strings.IndexByte(script[i+1:], '\'')
			if j < 0 {
				cur.WriteString(script[i+1:])
				i = len(script)
				break
			}
			cur.WriteString(script[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			quote()
			for i++; i < len(script) && script[i] != '"'; i++ {
				switch {
				case script[i] == '\\' && i+1 < len(script) && strings.IndexByte("\"\\$`\n", script[i+1]) >= 0:
					i++
				case strings.HasPrefix(script[i:], "$("):
					end := skipSubstitution(script, i+2)
					cur.WriteString(script[i:end])
					i = end - 1
					continue
				}
				cur.WriteByte(script[i])
			}
		case strings.HasPrefix(script[i:], "$("):
			// A command substitution runs in a subshell: its exports never
			// reach the caller, so its text is kept as opaque word data.
			inWord = true
			end := skipSubstitution(script, i+2)
			cur.WriteString(script[i:end])
			i = end - 1
		case c == '\\' && i+1 < len(script):
			inWord = true
			i++
			if script[i] != '\n' {
				cur.WriteByte(script[i])
			}
		case c == ' ' || c == '\t':
			endWord()
		case c == '\n' || c == ';':
			endCmd(";")
		case c == '&' || c == '|':
			sep := string(c)
			if i+1 < len(script) && (script[i+1] == c || (c == '|' && script[i+1] == '&')) {
				i++
				sep += string(script[i])
			}
			endCmd(sep)
		case c == '(' || (c == '{' && !inWord):
			endCmd(string(c))
			groupDepth++
		case c == ')' || (c == '}' && !inWord):
			endCmd(string(c))
			groupDepth = max(groupDepth-1, 0)
		case c == '#' && !inWord:
			for i < len(script) && script[i] != '\n' {
				i++
			}
			endCmd(";")
		default:
			inWord = true
			if c == '=' {
				sawEq = true
			}
			cur.WriteByte(c)
		}
	}
	endCmd("")
	return cmds
}

// skipSubstitution returns the index just past the ")" closing the command
// substitution whose body starts at i (just after "$("), honoring nested
// quotes, escapes, and parentheses. An unterminated substitution consumes
// the rest of the script.
func skipSubstitution(s string, i int) int {
	depth := 1
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return len(s)
			}
			i += j + 2
			continue
		case '"':
			i = skipDoubleQuoted(s, i+1)
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
		i++
	}
	return len(s)
}

// skipDoubleQuoted returns the index just past the '"' closing the
// double-quoted string whose body starts at i, descending into command
// substitutions so their own quotes do not end it.
func skipDoubleQuoted(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == '\\':
			i += 2
		case s[i] == '"':
			return i + 1
		case strings.HasPrefix(s[i:], "$("):
			i = skipSubstitution(s, i+2)
		default:
			i++
		}
	}
	return len(s)
}
