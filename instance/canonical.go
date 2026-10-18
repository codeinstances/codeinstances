// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package instance

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// The canonical form is written by hand rather than by encoding/json, so the
// id depends only on the rules in docs/FORMAT.md and not on how a Go release
// happens to escape strings. The parser is strict on purpose: it refuses
// invalid UTF-8, unpaired surrogate escapes and repeated member names, any of
// which would let two different files share an id, or one file be read two
// ways.

type jkind int

const (
	jnull jkind = iota
	jbool
	jnum
	jstr
	jarr
	jobj
)

type jval struct {
	kind  jkind
	b     bool
	s     string // string value, or a number exactly as written
	arr   []jval
	keys  []string
	items map[string]jval
}

type jparser struct {
	data []byte
	pos  int
}

func parseJSON(data []byte) (jval, error) {
	if !utf8.Valid(data) {
		return jval{}, errors.New("the file isn't valid UTF-8")
	}
	p := &jparser{data: data}
	p.space()
	v, err := p.value(0)
	if err != nil {
		return jval{}, err
	}
	p.space()
	if p.pos != len(p.data) {
		return jval{}, p.fail("data after the JSON document")
	}
	return v, nil
}

func (p *jparser) fail(msg string) error {
	return fmt.Errorf("JSON at byte %d: %s", p.pos, msg)
}

func (p *jparser) space() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jparser) value(depth int) (jval, error) {
	if depth > 200 {
		return jval{}, p.fail("nested too deeply")
	}
	if p.pos >= len(p.data) {
		return jval{}, p.fail("unexpected end")
	}
	switch c := p.data[p.pos]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		s, err := p.str()
		return jval{kind: jstr, s: s}, err
	case c == 't':
		return jval{kind: jbool, b: true}, p.word("true")
	case c == 'f':
		return jval{kind: jbool}, p.word("false")
	case c == 'n':
		return jval{kind: jnull}, p.word("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return jval{}, p.fail("unexpected character")
}

func (p *jparser) word(w string) error {
	if !bytes.HasPrefix(p.data[p.pos:], []byte(w)) {
		return p.fail("unexpected word")
	}
	p.pos += len(w)
	return nil
}

func (p *jparser) number() (jval, error) {
	start := p.pos
	digits := func() int {
		n := 0
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
			n++
		}
		return n
	}
	if p.data[p.pos] == '-' {
		p.pos++
	}
	if p.pos < len(p.data) && p.data[p.pos] == '0' {
		p.pos++
	} else if digits() == 0 {
		return jval{}, p.fail("bad number")
	}
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		p.pos++
		if digits() == 0 {
			return jval{}, p.fail("bad number")
		}
	}
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		if digits() == 0 {
			return jval{}, p.fail("bad number")
		}
	}
	return jval{kind: jnum, s: string(p.data[start:p.pos])}, nil
}

func (p *jparser) str() (string, error) {
	p.pos++ // the opening quote
	var b []byte
	for {
		if p.pos >= len(p.data) {
			return "", p.fail("unterminated string")
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return string(b), nil
		case c < 0x20:
			return "", p.fail("control character in a string")
		case c != '\\':
			b = append(b, c)
			p.pos++
			continue
		}
		p.pos++
		if p.pos >= len(p.data) {
			return "", p.fail("unterminated escape")
		}
		e := p.data[p.pos]
		p.pos++
		switch e {
		case '"', '\\', '/':
			b = append(b, e)
		case 'b':
			b = append(b, '\b')
		case 'f':
			b = append(b, '\f')
		case 'n':
			b = append(b, '\n')
		case 'r':
			b = append(b, '\r')
		case 't':
			b = append(b, '\t')
		case 'u':
			r, err := p.hex4()
			if err != nil {
				return "", err
			}
			if utf16.IsSurrogate(r) {
				if r >= 0xDC00 || !bytes.HasPrefix(p.data[p.pos:], []byte{'\\', 'u'}) {
					return "", p.fail("unpaired surrogate escape")
				}
				p.pos += 2
				r2, err := p.hex4()
				if err != nil {
					return "", err
				}
				r = utf16.DecodeRune(r, r2)
				if r == utf8.RuneError {
					return "", p.fail("unpaired surrogate escape")
				}
			}
			b = utf8.AppendRune(b, r)
		default:
			return "", p.fail("bad escape")
		}
	}
}

func (p *jparser) hex4() (rune, error) {
	if p.pos+4 > len(p.data) {
		return 0, p.fail("short \\u escape")
	}
	n, err := strconv.ParseUint(string(p.data[p.pos:p.pos+4]), 16, 32)
	if err != nil {
		return 0, p.fail("bad \\u escape")
	}
	p.pos += 4
	return rune(n), nil
}

func (p *jparser) array(depth int) (jval, error) {
	p.pos++
	v := jval{kind: jarr, arr: []jval{}}
	p.space()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return v, nil
	}
	for {
		p.space()
		item, err := p.value(depth + 1)
		if err != nil {
			return jval{}, err
		}
		v.arr = append(v.arr, item)
		p.space()
		if p.pos >= len(p.data) {
			return jval{}, p.fail("unterminated array")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return v, nil
		default:
			return jval{}, p.fail("expected , or ]")
		}
	}
}

func (p *jparser) object(depth int) (jval, error) {
	p.pos++
	v := jval{kind: jobj, items: map[string]jval{}}
	p.space()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return v, nil
	}
	for {
		p.space()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return jval{}, p.fail("expected a member name")
		}
		k, err := p.str()
		if err != nil {
			return jval{}, err
		}
		if _, dup := v.items[k]; dup {
			return jval{}, p.fail(fmt.Sprintf("member %q appears twice", k))
		}
		p.space()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return jval{}, p.fail("expected :")
		}
		p.pos++
		p.space()
		item, err := p.value(depth + 1)
		if err != nil {
			return jval{}, err
		}
		v.keys = append(v.keys, k)
		v.items[k] = item
		p.space()
		if p.pos >= len(p.data) {
			return jval{}, p.fail("unterminated object")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return v, nil
		default:
			return jval{}, p.fail("expected , or }")
		}
	}
}

// writeCanonical writes v by the rules in docs/FORMAT.md.
func writeCanonical(b *bytes.Buffer, v jval) {
	switch v.kind {
	case jnull:
		b.WriteString("null")
	case jbool:
		if v.b {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case jnum:
		b.WriteString(v.s)
	case jstr:
		writeString(b, v.s)
	case jarr:
		b.WriteByte('[')
		for i, item := range v.arr {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, item)
		}
		b.WriteByte(']')
	case jobj:
		keys := append([]string(nil), v.keys...)
		sort.Strings(keys) // byte order of the UTF-8 strings
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			writeCanonical(b, v.items[k])
		}
		b.WriteByte('}')
	}
}

const hexDigits = "0123456789abcdef"

func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20, r == 0x2028, r == 0x2029:
			b.WriteString(`\u`)
			b.WriteByte(hexDigits[r>>12&0xf])
			b.WriteByte(hexDigits[r>>8&0xf])
			b.WriteByte(hexDigits[r>>4&0xf])
			b.WriteByte(hexDigits[r&0xf])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// covers reports whether every member of want is in have with the same value,
// recursively; arrays must match element for element. It checks that the
// members Go read into the Instance struct are exactly the ones in the file,
// so no reader can see a different instance than the id covers.
func covers(have, want jval, path string) error {
	switch {
	case want.kind == jobj && have.kind == jobj:
		for _, k := range want.keys {
			h, ok := have.items[k]
			if !ok {
				return fmt.Errorf("%s: the member %q isn't spelled as the format says", path, k)
			}
			if err := covers(h, want.items[k], path+"."+k); err != nil {
				return err
			}
		}
		return nil
	case want.kind == jarr && have.kind == jarr:
		if len(want.arr) != len(have.arr) {
			return fmt.Errorf("%s: array length differs", path)
		}
		for i := range want.arr {
			if err := covers(have.arr[i], want.arr[i], fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	case want.kind == jnull && (have.kind == jarr && len(have.arr) == 0 || have.kind == jobj && len(have.keys) == 0):
		return nil
	case want.kind != have.kind:
		return fmt.Errorf("%s: unexpected type", path)
	case want.kind == jnum:
		if want.s != have.s {
			return fmt.Errorf("%s: number written as %s, read as %s", path, have.s, want.s)
		}
		return nil
	case want.kind == jstr && want.s != have.s, want.kind == jbool && want.b != have.b:
		return fmt.Errorf("%s: value differs", path)
	}
	return nil
}
