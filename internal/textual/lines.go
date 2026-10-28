// Package textual detects real textual conflicts between workstreams with
// git merge-tree. This file holds the pure half: parsing -U0 hunks, mapping
// line numbers between a file and its resolved variant, and resolving diff3
// conflict blocks toward one side. It does no I/O.
package textual

import (
	"bytes"
	"errors"
	"strconv"
	"strings"

	"github.com/joacofg/dugout/internal/core"
)

// MarkerSize is the length of every conflict marker (conflict-marker-size).
const MarkerSize = 32

// Hunk is one `@@ -OldStart,OldLen +NewStart,NewLen @@` header of a -U0 diff.
type Hunk struct{ OldStart, OldLen, NewStart, NewLen int }

// ParseHunks returns the hunk headers of a `git diff -U0`, in order. Every
// other line is ignored.
func ParseHunks(diff string) ([]Hunk, error) {
	var hunks []Hunk
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "@@ -") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "@@" || fields[2][0] != '+' || fields[3] != "@@" {
			return nil, errors.New("malformed hunk header: " + line)
		}
		oldStart, oldLen, err := parseSpan(fields[1][1:])
		if err != nil {
			return nil, errors.New("malformed hunk header: " + line)
		}
		newStart, newLen, err := parseSpan(fields[2][1:])
		if err != nil {
			return nil, errors.New("malformed hunk header: " + line)
		}
		hunks = append(hunks, Hunk{oldStart, oldLen, newStart, newLen})
	}
	return hunks, nil
}

// parseSpan reads "a" (length 1) or "a,b".
func parseSpan(s string) (start, n int, err error) {
	a, b, hasLen := strings.Cut(s, ",")
	if start, err = strconv.Atoi(a); err != nil {
		return 0, 0, err
	}
	n = 1
	if hasLen {
		if n, err = strconv.Atoi(b); err != nil {
			return 0, 0, err
		}
	}
	return start, n, nil
}

// MapToOld carries a range in "new" coordinates back to "old". It maps the
// first and last line and returns the old range covering both; a point
// (Len == 0) stays a point.
func MapToOld(hunks []Hunk, r core.Range) core.Range {
	if r.Len == 0 {
		start, _ := mapLine(hunks, r.Start)
		return core.Range{Start: start, Len: 0}
	}
	first, firstEnd := mapLine(hunks, r.Start)
	_, lastEnd := mapLine(hunks, r.Start+r.Len-1)
	end := max(firstEnd, lastEnd)
	return core.Range{Start: first, Len: max(end-first+1, 0)}
}

// mapLine maps one new line to its old span [start, end]; end < start when
// the line falls on a pure insertion (a point before start).
func mapLine(hunks []Hunk, n int) (start, end int) {
	shift := 0
	for _, h := range hunks {
		switch {
		case h.NewLen > 0 && n >= h.NewStart && n < h.NewStart+h.NewLen:
			if h.OldLen == 0 {
				return h.OldStart + 1, h.OldStart
			}
			return h.OldStart, h.OldStart + h.OldLen - 1
		case h.NewLen > 0 && n >= h.NewStart+h.NewLen, h.NewLen == 0 && n > h.NewStart:
			shift += h.OldLen - h.NewLen
		}
	}
	return n + shift, n + shift
}

// Resolve replaces every diff3 conflict block by its section for side (0 =
// ours, 1 = theirs) and returns where each section landed in resolved. A
// section's range is 1-based; an empty section is a point (Len 0) at the line
// that follows it. An unclosed block is an error.
func Resolve(merged []byte, side int) (resolved []byte, blocks []core.Range, err error) {
	const (
		text = iota
		ours
		base
		theirs
	)
	state := text
	var out bytes.Buffer
	lines := 0        // lines written to out so far
	sectionStart := 0 // first line of the section being kept
	for len(merged) > 0 {
		var line []byte
		if i := bytes.IndexByte(merged, '\n'); i >= 0 {
			line, merged = merged[:i+1], merged[i+1:]
		} else {
			line, merged = merged, nil
		}
		switch marker(line) {
		case '<':
			if state == text {
				state = ours
				sectionStart = lines + 1
				continue
			}
		case '|':
			if state == ours {
				state = base
				if side == 0 {
					blocks = append(blocks, core.Range{Start: sectionStart, Len: lines + 1 - sectionStart})
				}
				continue
			}
		case '=':
			if state == ours || state == base {
				if state == ours && side == 0 {
					blocks = append(blocks, core.Range{Start: sectionStart, Len: lines + 1 - sectionStart})
				}
				state = theirs
				sectionStart = lines + 1
				continue
			}
		case '>':
			if state == theirs {
				state = text
				if side == 1 {
					blocks = append(blocks, core.Range{Start: sectionStart, Len: lines + 1 - sectionStart})
				}
				continue
			}
		}
		// Plain text, or a marker out of place, which stays as text.
		if state == text || state == ours && side == 0 || state == theirs && side == 1 {
			out.Write(line)
			lines++
		}
	}
	if state != text {
		return nil, nil, errors.New("unclosed conflict block")
	}
	return out.Bytes(), blocks, nil
}

// Caps on what Sections keeps: blocks per file, lines per block, runes per
// line.
const (
	MaxSnippets     = 3
	MaxSnippetLines = 12
	MaxSnippetRunes = 200
)

// Sections cuts out of resolved the lines of the first MaxSnippets blocks,
// as Resolve ranged them, and returns how many blocks it left out. Each
// section keeps up to MaxSnippetLines lines and counts the rest in More; an
// empty section (Len 0) has no lines. Lines carry no end of line, and one
// longer than MaxSnippetRunes is cut, the cut marked with "…".
func Sections(resolved []byte, blocks []core.Range) ([]core.Snippet, int) {
	lines := strings.SplitAfter(string(resolved), "\n")
	out := make([]core.Snippet, 0, min(len(blocks), MaxSnippets))
	for _, b := range blocks[:min(len(blocks), MaxSnippets)] {
		s := core.Snippet{Lines: []string{}}
		for i := 0; i < b.Len; i++ {
			if i >= MaxSnippetLines {
				s.More = b.Len - MaxSnippetLines
				break
			}
			s.Lines = append(s.Lines, cut(strings.TrimRight(lines[b.Start-1+i], "\r\n")))
		}
		out = append(out, s)
	}
	return out, max(len(blocks)-MaxSnippets, 0)
}

// cut keeps a line of up to MaxSnippetRunes runes as is, and of a longer one
// its first MaxSnippetRunes-1 runes and "…". It stops at the rune past the
// cap, so a minified megabyte line is not read to its end.
func cut(line string) string {
	n, end := 0, 0
	for i := range line {
		switch n {
		case MaxSnippetRunes - 1:
			end = i
		case MaxSnippetRunes:
			return line[:end] + "…"
		}
		n++
	}
	return line
}

// marker reports the marker character of a line made of exactly MarkerSize
// equal marker characters followed by end of line or a space, else 0.
func marker(line []byte) byte {
	if len(line) < MarkerSize {
		return 0
	}
	c := line[0]
	if c != '<' && c != '|' && c != '=' && c != '>' {
		return 0
	}
	for _, b := range line[:MarkerSize] {
		if b != c {
			return 0
		}
	}
	if rest := line[MarkerSize:]; len(rest) > 0 && rest[0] != ' ' && rest[0] != '\n' && rest[0] != '\r' {
		return 0
	}
	return c
}
