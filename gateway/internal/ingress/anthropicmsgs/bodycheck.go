package anthropicmsgs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxBodyDepth bounds how deeply a request body may nest (every "{" or "["
// counts one level; the body object itself is level 1). A Claude Code turn
// is about eight levels deep and a tool's input_schema sits three levels
// in, so the adapter's own 32-level schema bound (adapter.validate.go)
// fits inside this with room to spare; the handler's byte limit is the
// primary guard and this is defense in depth against a deliberately deep
// document reaching the recursive decoders.
const maxBodyDepth = 64

// BodyError is checkBody's error: errors.Is(err, ErrInvalidBody) holds and
// Error() is the sentinel's text followed by Detail -- exactly the message the
// Anthropic handler relays. Detail (a JSON pointer and member name, a depth,
// "not valid UTF-8", or a decoder error that never echoes the body) is
// exposed for a caller that formats its own message: the OpenAI routes keep
// their `invalid request body: ` prefix and never show the sentinel.
type BodyError struct{ Detail string }

func (e *BodyError) Error() string { return ErrInvalidBody.Error() + ": " + e.Detail }

// Is makes errors.Is(err, ErrInvalidBody) true, as the %w wrap it replaced did.
func (e *BodyError) Is(target error) bool { return target == ErrInvalidBody }

func bodyError(format string, args ...any) error {
	return &BodyError{Detail: fmt.Sprintf(format, args...)}
}

// CheckBody is checkBody for the OpenAI decode paths (round-5 item 7,
// 2026-10-15): the same three refusals before encoding/json runs, so no
// ingress route silently reinterprets a body (a duplicate member kept
// last-wins, U+FFFD put where the UTF-8 was invalid, unbounded nesting). A
// non-object body passes, and so does an out-of-range number literal:
// object() and the typed decoders name those cases on each route.
func CheckBody(body []byte) error { return checkBody(body) }

// bodyFrame is one open object or array during checkBody's token walk.
type bodyFrame struct {
	isObject bool
	keys     map[string]struct{} // members seen so far (objects)
	key      string              // the member whose value is being read (objects)
	index    int                 // the element being read (arrays)
	wantKey  bool                // the next token is a member name (objects)
}

// checkBody walks the body once as tokens and refuses the shapes the shadow
// could otherwise silently disagree with the raw bytes on: a member that
// appears twice in one object (encoding/json keeps the last value; a
// passthrough upstream parsing the same bytes may keep the first, so the
// guardrail would have scanned a different request from the one the model
// reads), nesting past maxBodyDepth, and invalid UTF-8 (encoding/json would
// put U+FFFD in the shadow while the raw bytes travel on). Syntax errors
// surface here too, as a BodyError (errors.Is ErrInvalidBody) that never
// echoes the body.
func checkBody(body []byte) error {
	if !utf8.Valid(body) {
		return bodyError("not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	// Tokens, not values: without UseNumber the decoder parses every number
	// literal and an out-of-range one (1e999) would be refused here as a type
	// error; that is the typed decoder's business, like a non-object body.
	dec.UseNumber()
	var stack []*bodyFrame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return bodyError("%s", describe(err))
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if len(stack) >= maxBodyDepth {
					return bodyError("nests deeper than %d levels", maxBodyDepth)
				}
				f := &bodyFrame{isObject: d == '{', wantKey: d == '{'}
				if f.isObject {
					f.keys = map[string]struct{}{}
				}
				stack = append(stack, f)
			case '}', ']':
				stack = stack[:len(stack)-1]
				completeValue(stack)
			}
			continue
		}
		if len(stack) == 0 {
			continue // a bare scalar body; object() rejects it with a clearer message
		}
		top := stack[len(stack)-1]
		if top.isObject && top.wantKey {
			key, _ := tok.(string) // encoding/json only yields strings in key position
			if _, dup := top.keys[key]; dup {
				return bodyError("%s has a duplicate member %q", ptrOrBody(pointerOf(stack[:len(stack)-1])), truncate(key))
			}
			top.keys[key] = struct{}{}
			top.key = key
			top.wantKey = false
			continue
		}
		completeValue(stack)
	}
}

// completeValue marks the innermost open container's current member or
// element as finished: an object expects the next member name, an array
// moves to the next index.
func completeValue(stack []*bodyFrame) {
	if len(stack) == 0 {
		return
	}
	top := stack[len(stack)-1]
	if top.isObject {
		top.wantKey = true
		return
	}
	top.index++
}

// pointerOf renders the path to the innermost frame as a JSON pointer: the
// member being read for an object (truncated: it goes into an error
// message), the element index for an array.
func pointerOf(frames []*bodyFrame) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteByte('/')
		if f.isObject {
			b.WriteString(escapePointer(truncate(f.key)))
		} else {
			b.WriteString(strconv.Itoa(f.index))
		}
	}
	return b.String()
}
