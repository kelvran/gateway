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
// surface here too, wrapped in ErrInvalidBody and never echoing the body.
func checkBody(body []byte) error {
	if !utf8.Valid(body) {
		return fmt.Errorf("%w: not valid UTF-8", ErrInvalidBody)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var stack []*bodyFrame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidBody, describe(err))
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if len(stack) >= maxBodyDepth {
					return fmt.Errorf("%w: nests deeper than %d levels", ErrInvalidBody, maxBodyDepth)
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
				return fmt.Errorf("%w: %s has a duplicate member %q", ErrInvalidBody, ptrOrBody(pointerOf(stack[:len(stack)-1])), truncate(key))
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
