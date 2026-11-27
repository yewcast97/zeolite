package acpbridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
)

// object is a JSON object as this package reads and writes one: each
// member under its exact name.
type object = map[string]any

// maxDepth is how deep a message's values may stand in one another:
// deeper than any message of the protocol goes.
const maxDepth = 128

// maxIDBytes bounds a request's id that is a string.
const maxIDBytes = 256

// decode reads a line as one JSON object. Its members are kept under
// their exact names: a name that differs from a known one by its case
// is another name, and an object that has a name twice is refused, so
// that what this package judged is what whoever reads the message next
// will find. Numbers are kept as they were written.
func decode(line []byte) (object, bool) {
	if !json.Valid(line) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	v, err := decodeValue(dec, 0)
	if err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	o, ok := v.(object)
	return o, ok
}

var errNotTaken = errors.New("a value that is not taken")

func decodeValue(dec *json.Decoder, depth int) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, opens := tok.(json.Delim)
	if !opens {
		return tok, nil // a string, a number, a boolean or null
	}
	if depth >= maxDepth {
		return nil, errNotTaken
	}
	switch delim {
	case '{':
		o := object{}
		for dec.More() {
			nameTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			name, ok := nameTok.(string)
			if !ok {
				return nil, errNotTaken
			}
			if _, twice := o[name]; twice {
				return nil, errNotTaken
			}
			if o[name], err = decodeValue(dec, depth+1); err != nil {
				return nil, err
			}
		}
		_, err := dec.Token()
		return o, err
	case '[':
		list := []any{}
		for dec.More() {
			v, err := decodeValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		_, err := dec.Token()
		return list, err
	}
	return nil, errNotTaken
}

// write encodes a message on one line, with its end.
func write(m object) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// What a message is, in JSON-RPC's terms.
type messageKind int

const (
	isRequest messageKind = iota + 1
	isNotification
	isResult
	isFailure
)

// message is one message of the protocol as it was read: what it is,
// and its parts, none of them judged yet.
type message struct {
	kind messageKind
	// id is the id of a request or of its answer, a string or an
	// integer, and key what tells it from every other id.
	id  any
	key string
	// method and params are a request's or a notification's. params is
	// nil where the message had none.
	method string
	params object
	// result is a result's value, and failure an error's object.
	result  any
	failure object
}

// read reads a line as one message. A line that is not one JSON
// object, as a batch is not, is none; nor is a message that is both a
// call and an answer, one whose id is neither a string nor an integer,
// or one whose parameters are not an object.
func read(line []byte) (message, bool) {
	o, ok := decode(line)
	if !ok {
		return message{}, false
	}
	var m message
	rawID, hasID := o["id"]
	_, hasResult := o["result"]
	_, hasFailure := o["error"]
	if hasID {
		if m.id, m.key, ok = requestID(rawID); !ok {
			return message{}, false
		}
	}
	if rawMethod, called := o["method"]; called {
		if hasResult || hasFailure {
			return message{}, false
		}
		if m.method, ok = rawMethod.(string); !ok {
			return message{}, false
		}
		switch params := o["params"].(type) {
		case nil:
		case object:
			m.params = params
		default:
			return message{}, false
		}
		m.kind = isNotification
		if hasID {
			m.kind = isRequest
		}
		return m, true
	}
	switch {
	case !hasID || hasResult == hasFailure:
		return message{}, false
	case hasResult:
		m.kind, m.result = isResult, o["result"]
	default:
		if m.failure, ok = o["error"].(object); !ok {
			return message{}, false
		}
		m.kind = isFailure
	}
	return m, true
}

// requestID reads a request's id: a string of a bounded length, or an
// integer. It returns the id as it is written again, and a key that no
// other id has.
func requestID(v any) (id any, key string, ok bool) {
	switch v := v.(type) {
	case string:
		if len(v) > maxIDBytes {
			return nil, "", false
		}
		return v, "s" + v, true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return nil, "", false
		}
		return n, "i" + strconv.FormatInt(n, 10), true
	}
	return nil, "", false
}

func request(id any, method string, params object) object {
	return object{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
}

func notification(method string, params object) object {
	return object{"jsonrpc": "2.0", "method": method, "params": params}
}

func result(id any, value object) object {
	return object{"jsonrpc": "2.0", "id": id, "result": value}
}

func failure(id any, r refusal) object {
	e := object{"code": r.code, "message": r.message}
	if r.data != nil {
		e["data"] = r.data
	}
	return object{"jsonrpc": "2.0", "id": id, "error": e}
}

// The members of an object, each taken only as the type the protocol
// gives it.

func text(o object, name string) (string, bool) {
	s, ok := o[name].(string)
	return s, ok
}

func member(o object, name string) (object, bool) {
	m, ok := o[name].(object)
	return m, ok
}

func items(o object, name string) ([]any, bool) {
	list, ok := o[name].([]any)
	return list, ok
}

// whole reads a member that is an integer and not below zero.
func whole(o object, name string) (int64, bool) {
	n, ok := o[name].(json.Number)
	if !ok {
		return 0, false
	}
	v, err := n.Int64()
	return v, err == nil && v >= 0
}

// keepText copies the members named that are strings, and leaves out
// the others.
func keepText(out, in object, names ...string) {
	for _, name := range names {
		if s, ok := in[name].(string); ok {
			out[name] = s
		}
	}
}

// needText copies the members named, and says whether each was there
// as a string.
func needText(out, in object, names ...string) bool {
	all := true
	for _, name := range names {
		s, ok := in[name].(string)
		if !ok {
			all = false
			continue
		}
		out[name] = s
	}
	return all
}

// keepFlags copies the members named that are booleans.
func keepFlags(out, in object, names ...string) {
	for _, name := range names {
		if b, ok := in[name].(bool); ok {
			out[name] = b
		}
	}
}

// keepWhole copies the members named that are integers not below zero.
func keepWhole(out, in object, names ...string) {
	for _, name := range names {
		if n, ok := whole(in, name); ok {
			out[name] = n
		}
	}
}

// keepNumbers copies the members named that are numbers, as written.
func keepNumbers(out, in object, names ...string) {
	for _, name := range names {
		if n, ok := in[name].(json.Number); ok {
			out[name] = n
		}
	}
}

// keepMarks says of each member named whether the object had it as an
// object: what a capability that carries nothing is.
func keepMarks(out, in object, names ...string) {
	for _, name := range names {
		if _, ok := in[name].(object); ok {
			out[name] = object{}
		}
	}
}

// plain copies a value whose shape the protocol leaves open, as an
// agent's own account of what a tool was given and gave: every part of
// it written anew, and no member named _meta at any depth.
func plain(v any) any {
	switch v := v.(type) {
	case object:
		out := object{}
		for name, value := range v {
			if name != metaMember {
				out[name] = plain(value)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, value := range v {
			out[i] = plain(value)
		}
		return out
	}
	return v
}

// metaMember is the member by which the protocol lets either side
// attach what it has no member for. None crosses.
const metaMember = "_meta"
