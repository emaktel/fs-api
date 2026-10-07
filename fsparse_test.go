package main

import (
	"strings"
	"testing"
)

// A port of the FreeSWITCH 1.10.12 string splitting that `api originate`
// runs its input through (src/switch_utils.c switch_separate_string,
// separate_string_blank_delim, separate_string_char_delim,
// cleanup_separated_string, unescape_char; src/switch_event.c
// switch_event_create_brackets for the {..} list), so tests can check what
// FreeSWITCH would actually read from a command fs-api builds.

const fsEscapeMeta = '\\'

func fsUnescapeChar(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	case 's':
		return ' '
	}
	return c
}

// fsCleanup ports cleanup_separated_string: strip quotes, escapes and
// leading/trailing spaces.
func fsCleanup(str []byte, delim byte) string {
	p := 0
	for p < len(str) && str[p] == ' ' {
		p++
	}
	var dest []byte
	end := -1
	inQuotes := false
	for ; p < len(str); p++ {
		esc := false
		if str[p] == fsEscapeMeta && p+1 < len(str) {
			e := str[p+1]
			if e == '\'' || e == '"' || (delim != 0 && e == delim) || e == fsEscapeMeta || fsUnescapeChar(e) != e {
				p++
				dest = append(dest, fsUnescapeChar(e))
				end = len(dest)
				esc = true
			}
		}
		if !esc {
			if str[p] == '\'' && (inQuotes || strings.IndexByte(string(str[p+1:]), '\'') >= 0) {
				inQuotes = !inQuotes
				if inQuotes {
					end = len(dest)
				}
			} else {
				dest = append(dest, str[p])
				if str[p] != ' ' || inQuotes {
					end = len(dest)
				}
			}
		}
	}
	if end < 0 {
		return string(dest)
	}
	return string(dest[:end])
}

// fsSeparate ports switch_separate_string (including the "^^x" delimiter switch).
func fsSeparate(s string, delim byte, arraylen int) []string {
	buf := []byte(s)
	if len(buf) >= 3 && buf[0] == '^' && buf[1] == '^' {
		delim = buf[2]
		buf = buf[3:]
	}
	var bounds [][2]int
	ptr := 0
	if delim == ' ' {
		const (
			start = iota
			skipInitial
			findDelim
			skipEnding
		)
		state, inQuotes, tokStart := start, false, 0
		for ptr < len(buf) && len(bounds) < arraylen {
			switch state {
			case start:
				tokStart = ptr
				bounds = append(bounds, [2]int{tokStart, len(buf)})
				state = skipInitial
			case skipInitial:
				if buf[ptr] == ' ' {
					ptr++
				} else {
					state = findDelim
				}
			case findDelim:
				if buf[ptr] == fsEscapeMeta {
					ptr++
				} else if buf[ptr] == '\'' {
					inQuotes = !inQuotes
				} else if buf[ptr] == ' ' && !inQuotes {
					bounds[len(bounds)-1][1] = ptr
					state = skipEnding
				}
				ptr++
			case skipEnding:
				if buf[ptr] == ' ' {
					ptr++
				} else {
					state = start
				}
			}
		}
	} else {
		inQuotes := false
		started := false
		for ptr < len(buf) && len(bounds) < arraylen {
			if !started {
				bounds = append(bounds, [2]int{ptr, len(buf)})
				started = true
				continue
			}
			if buf[ptr] == fsEscapeMeta {
				ptr++
			} else if buf[ptr] == '\'' && (inQuotes || strings.IndexByte(string(buf[ptr+1:]), '\'') >= 0) {
				inQuotes = !inQuotes
			} else if buf[ptr] == delim && !inQuotes {
				bounds[len(bounds)-1][1] = ptr
				started = false
			}
			ptr++
		}
	}
	out := make([]string, len(bounds))
	for i, b := range bounds {
		end := b[1]
		if end > len(buf) {
			end = len(buf)
		}
		out[i] = fsCleanup(buf[b[0]:end], map[bool]byte{true: 0, false: delim}[delim == ' '])
	}
	return out
}

// fsOriginate is what originate_function and switch_ivr_originate read from
// an `api originate` argument string: the positional arguments, and the
// variables of the A-leg's leading {..} list.
type fsOriginate struct {
	args []string
	vars map[string]string
	leg  string
}

func fsParseOriginate(t *testing.T, cmd string) fsOriginate {
	t.Helper()
	rest, ok := strings.CutPrefix(cmd, "originate ")
	if !ok {
		t.Fatalf("not an originate: %q", cmd)
	}
	got := fsOriginate{args: fsSeparate(rest, ' ', 10), vars: map[string]string{}}
	if len(got.args) == 0 {
		return got
	}
	aleg := got.args[0]
	if !strings.HasPrefix(aleg, "{") {
		got.leg = aleg
		return got
	}
	end := strings.IndexByte(aleg, '}') // fields can't contain braces
	if end < 0 {
		t.Fatalf("unterminated {..} in %q", aleg)
	}
	for _, v := range fsSeparate(aleg[1:end], ',', 1024) {
		if kv := fsSeparate(v, '=', 2); len(kv) == 2 {
			got.vars[kv[0]] = kv[1]
		}
	}
	got.leg = aleg[end+1:]
	return got
}

// fsOriginateArgs is the positional arguments FreeSWITCH reads.
func fsOriginateArgs(t *testing.T, cmd string) []string {
	t.Helper()
	return fsParseOriginate(t, cmd).args
}

// The port itself, against cases worked through the C source by hand.
func TestFSParsePort(t *testing.T) {
	if got := fsSeparate(`a 'b c' d\s e`, ' ', 10); strings.Join(got, "|") != "a|b c|d |e" {
		t.Fatalf("blank split = %q", got)
	}
	if got := fsSeparate(`x=1,y=a\,b,z='p,q'`, ',', 10); strings.Join(got, "|") != "x=1|y=a,b|z=p,q" {
		t.Fatalf("comma split = %q", got)
	}
	if got := fsSeparate(`k=v=w`, '=', 2); strings.Join(got, "|") != "k|v=w" {
		t.Fatalf("= split = %q", got)
	}
	if got := fsSeparate(`^^:a:b c`, ' ', 10); strings.Join(got, "|") != "a|b c" {
		t.Fatalf("^^ switch = %q", got)
	}
	if got := fsCleanup([]byte(`D'Arcy O'Neil`), '='); got != "DArcy ONeil" {
		t.Fatalf("paired apostrophes are quotes: %q", got)
	}
}
