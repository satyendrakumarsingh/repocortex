package infra

import (
	"fmt"
	"strconv"
	"strings"
)

// A Dockerfile is line-oriented, so this is a line scanner rather than a parser
// — and unlike the YAML formats there is no library to defer to. What it has to
// get right is small and specific: line continuations, comments inside a
// continuation, and the parser directive that changes the continuation
// character. Everything else is a first token and a remainder.
//
// It reads FROM, EXPOSE, ENV, ARG, ENTRYPOINT and CMD. RUN is deliberately not
// read at all: build commands are where `curl -H "Authorization: Bearer …"`
// lives, and the safest handling of a line whose content this package has no
// use for is to never look at it.

// maxLogicalLines bounds the instruction count of one Dockerfile.
const maxLogicalLines = 20000

func parseDockerfile(path string, src []byte) File {
	f := File{Path: path, Format: FormatDockerfile}

	lines := strings.Split(string(src), "\n")
	escape := dockerEscape(lines)

	var (
		units   []Unit
		pending []EnvKey // ARG declared before the first FROM: build-global
		stageN  int
	)

	// cur points at the stage instructions currently attach to. Nil until the
	// first FROM, because an instruction before FROM belongs to no image.
	var cur *Unit

	for i := 0; i < len(lines); i++ {
		if len(units) >= maxUnits {
			f.Problems = append(f.Problems, Problem{
				Reason: ReasonLimit,
				Detail: fmt.Sprintf("stopped after %d stages", maxUnits),
			})
			break
		}
		if i >= maxLogicalLines {
			f.Problems = append(f.Problems, Problem{
				Reason: ReasonLimit,
				Detail: fmt.Sprintf("stopped after %d lines", maxLogicalLines),
			})
			break
		}

		raw := strings.TrimSpace(lines[i])
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		startLine := i + 1
		joined, next := joinContinuation(lines, i, escape)
		i = next

		instr, rest := splitInstruction(joined)
		switch instr {
		case "FROM":
			stageN++
			u := fromInstruction(rest, stageN, startLine)
			// ARGs seen before any FROM are global build arguments; they are
			// visible to every stage, so they attach to the first one rather
			// than being dropped.
			u.Env = append(u.Env, pending...)
			pending = nil
			units = append(units, u)
			cur = &units[len(units)-1]

		case "EXPOSE":
			if cur == nil {
				continue
			}
			cur.Ports = append(cur.Ports, exposePorts(rest, startLine)...)

		case "ENV", "ARG":
			from := SourceImage
			if instr == "ARG" {
				from = SourceBuildArg
			}
			keys := envInstruction(rest, startLine, from)
			if cur == nil {
				pending = append(pending, keys...)
				continue
			}
			if len(cur.Env)+len(keys) <= maxKeys {
				cur.Env = append(cur.Env, keys...)
			}

		case "ENTRYPOINT", "CMD":
			if cur == nil || cur.Command != "" && instr == "CMD" {
				// ENTRYPOINT wins: when both are present CMD supplies arguments
				// to it, and reporting the arguments as the command would name
				// the wrong process.
				continue
			}
			cur.Command = safe(rest)
		}
	}

	if len(units) == 0 {
		// A Dockerfile with no FROM builds nothing. Reported rather than
		// returned as an empty file, because "this repository has a Dockerfile
		// that declares no image" is a finding and silence is not.
		f.Problems = append(f.Problems, Problem{Reason: ReasonNoFrom})
		return f
	}
	units[len(units)-1].Final = true
	f.Units = units
	return f
}

// dockerEscape reads the `# escape=` parser directive, which must appear before
// any instruction. Windows Dockerfiles set it to a backtick, and a scanner that
// assumed a backslash would treat every continued line as a separate
// instruction.
func dockerEscape(lines []string) byte {
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if !strings.HasPrefix(t, "#") {
			return '\\' // directives are over at the first instruction
		}
		d := strings.TrimSpace(strings.TrimPrefix(t, "#"))
		if v, ok := strings.CutPrefix(d, "escape="); ok {
			if v = strings.TrimSpace(v); len(v) == 1 && (v[0] == '`' || v[0] == '\\') {
				return v[0]
			}
		}
	}
	return '\\'
}

// joinContinuation folds a continued instruction into one logical line and
// returns the index of its last physical line.
//
// Comment lines inside a continuation are dropped, which is what Docker itself
// does — and getting it wrong means an instruction silently loses the arguments
// that followed the comment.
func joinContinuation(lines []string, i int, escape byte) (string, int) {
	var b strings.Builder
	for ; i < len(lines); i++ {
		t := strings.TrimRight(lines[i], " \t\r")
		if b.Len() > 0 && strings.HasPrefix(strings.TrimSpace(t), "#") {
			continue
		}
		cont := strings.HasSuffix(t, string(escape))
		if cont {
			t = t[:len(t)-1]
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strings.TrimSpace(t))
		if !cont {
			break
		}
		if b.Len() > maxFieldBytes*32 {
			break // a continuation that never ends is not an instruction
		}
	}
	if i >= len(lines) {
		i = len(lines) - 1
	}
	return b.String(), i
}

func splitInstruction(line string) (instr, rest string) {
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return strings.ToUpper(line), ""
	}
	return strings.ToUpper(line[:i]), strings.TrimSpace(line[i+1:])
}

// fromInstruction reads `FROM [--platform=…] image[:tag] [AS name]`.
func fromInstruction(rest string, n, line int) Unit {
	u := Unit{Type: "stage", Line: line, Name: "stage" + strconv.Itoa(n)}
	toks := shellTokens(rest)
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if strings.HasPrefix(t, "--") {
			continue // --platform, --link and whatever is added next
		}
		if u.Image == "" {
			u.Image = safe(t)
			continue
		}
		if strings.EqualFold(t, "as") && i+1 < len(toks) {
			if name, ok := cleanName(toks[i+1]); ok {
				u.Name = name
			}
			break
		}
	}
	return u
}

// exposePorts reads `EXPOSE 8080 9090/udp`.
func exposePorts(rest string, line int) []Port {
	var out []Port
	for _, t := range shellTokens(rest) {
		port, proto := t, ""
		if i := strings.Index(t, "/"); i >= 0 {
			port, proto = t[:i], strings.ToLower(t[i+1:])
		}
		if proto != "tcp" && proto != "udp" {
			proto = ""
		}
		p := Port{Protocol: proto, Line: line}
		if v, err := strconv.Atoi(port); err == nil && v > 0 && v <= 65535 {
			p.Container = v
		} else {
			// `EXPOSE ${PORT}` and `EXPOSE 8000-8010` are real and resolve at
			// build or run time. Reported as written rather than dropped: a
			// port this scanner cannot resolve is still a port the image opens.
			p.Raw = safe(t)
			if p.Raw == "" {
				continue
			}
		}
		out = append(out, p)
		if len(out) > 256 {
			break
		}
	}
	return out
}

// envInstruction reads both ENV forms and both ARG forms, and keeps only names.
//
// `ENV K=V K2=V2` and the legacy `ENV K V rest of line`. In both the split is on
// the FIRST `=` or the first space, and the right-hand side is discarded in the
// same expression that produced it — it is never assigned to anything that
// outlives this loop.
func envInstruction(rest string, line int, from EnvSource) []EnvKey {
	toks := shellTokens(rest)
	if len(toks) == 0 {
		return nil
	}
	if !strings.Contains(toks[0], "=") {
		// Legacy form: the whole remainder after the first token is one value.
		if k, ok := newEnvKey(toks[0], line, from, ""); ok {
			return []EnvKey{k}
		}
		return nil
	}
	var out []EnvKey
	for _, t := range toks {
		name, _, found := strings.Cut(t, "=")
		if !found {
			continue
		}
		if k, ok := newEnvKey(name, line, from, ""); ok {
			out = append(out, k)
		}
		if len(out) >= 512 {
			break
		}
	}
	return out
}

// shellTokens splits on whitespace, honouring quotes and backslash escapes.
//
// Not a shell: it does no expansion, no globbing and no substitution, because
// the only thing it is used for is finding the boundaries of names.
func shellTokens(s string) []string {
	var (
		out  []string
		cur  strings.Builder
		q    byte
		open bool
	)
	flush := func() {
		if open || cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
		open = false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
				continue
			}
			if c == '\\' && q == '"' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				continue
			}
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			q, open = c, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case c == ' ' || c == '\t':
			flush()
		default:
			cur.WriteByte(c)
		}
		if len(out) >= 4096 {
			return out
		}
	}
	flush()
	return out
}
