package otel

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// redactedFieldNames are the needles a field name is matched against before its
// value is allowed to reach a log sink.
//
// # The shared redaction contract
//
// otel-instrumentation-go, otel-instrumentation-next and Netdb.Observability
// implement the same four rules. A secret that leaks in one language must leak
// in all three, or the contract has drifted and the weakest service decides
// what ends up in the log backend:
//
//  1. A name is normalised before matching - lowercased, with '-', '_', '.'
//     and ' ' removed. "api_key", "apiKey", "X-API-KEY" and "Api.Key" all
//     reduce to "apikey". HTTP header names are hyphenated and headers are the
//     most common accidental leak, so matching only the underscore spelling
//     would miss them.
//  2. The normalised name is substring-matched against this list. The usual
//     leak is a field that gained a secret months after the logging call was
//     written, so "sessionToken" and "db_credential" match too.
//  3. A match replaces the entire value with redactedPlaceholder, whatever the
//     value's type - the contents of a matching key are never inspected.
//  4. Matching applies at every depth up to maxRedactDepth, not only to
//     top-level fields. Logging a struct or map with a nested "password"
//     censors that field rather than the whole object.
var redactedFieldNames = []string{
	"password",
	"passwd",
	"secret",
	"token",
	"apikey",
	"authorization",
	"cookie",
	"credential",
}

// redactedPlaceholder is the value substituted for a redacted field.
const redactedPlaceholder = "[redacted]"

// maxRedactDepth bounds the nested walk. A log line nested deeper than this is
// unreadable anyway, and the cap is what makes a cyclic structure terminate.
const maxRedactDepth = 8

// normaliseKey lowercases and strips the separators that distinguish
// snake_case, kebab-case, dotted and camelCase spellings of the same field.
func normaliseKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range strings.ToLower(key) {
		if r == '-' || r == '_' || r == '.' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ShouldRedact reports whether a field with this name must be censored. It is
// exported so a service writing its own encoder can apply the same rule rather
// than inventing a second, weaker one.
func ShouldRedact(key string) bool {
	normalised := normaliseKey(key)
	for _, needle := range redactedFieldNames {
		if strings.Contains(normalised, needle) {
			return true
		}
	}
	return false
}

// redactCore wraps a zapcore.Core and censors sensitive fields before they are
// encoded. It sits below the tee so both the console and the OTLP exporter see
// the redacted values - redacting in only one place is how secrets end up in
// exactly the backend you forgot about.
type redactCore struct {
	zapcore.Core
}

func (c redactCore) With(fields []zapcore.Field) zapcore.Core {
	return redactCore{c.Core.With(redactFields(fields))}
}

func (c redactCore) Check(entry zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return ce.AddCore(entry, c)
	}
	return ce
}

func (c redactCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	return c.Core.Write(entry, redactFields(fields))
}

// redactFields censors secret-named fields outright and rewrites the rest so
// that anything nested inside them is censored as it is encoded.
func redactFields(fields []zapcore.Field) []zapcore.Field {
	out := make([]zapcore.Field, len(fields))
	inSecretNamespace := false
	for i, f := range fields {
		if inSecretNamespace || ShouldRedact(f.Key) {
			// zap.Namespace has no closing call, so every field after a
			// secret-named namespace is inside that secret.
			if f.Type == zapcore.NamespaceType {
				inSecretNamespace = true
			}
			out[i] = zap.String(f.Key, redactedPlaceholder)
			continue
		}
		out[i] = redactNested(f)
	}
	return out
}

// redactNested rewrites a field whose own name is clean but whose value may
// carry secrets underneath it.
func redactNested(f zapcore.Field) zapcore.Field {
	switch f.Type {
	case zapcore.ReflectType:
		if v, changed := redactReflected(f.Interface, 0); changed {
			f.Interface = v
		}
	case zapcore.ObjectMarshalerType, zapcore.InlineMarshalerType:
		if m, ok := f.Interface.(zapcore.ObjectMarshaler); ok {
			f.Interface = redactingObjectMarshaler{m}
		}
	case zapcore.ArrayMarshalerType:
		if m, ok := f.Interface.(zapcore.ArrayMarshaler); ok {
			f.Interface = redactingArrayMarshaler{m}
		}
	}
	return f
}

// redactingObjectMarshaler re-marshals an object through an encoder that
// censors secret-named keys.
type redactingObjectMarshaler struct{ zapcore.ObjectMarshaler }

func (m redactingObjectMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	return m.ObjectMarshaler.MarshalLogObject(&redactingObjectEncoder{enc: enc})
}

// redactingArrayMarshaler keeps the walk going through array elements, which
// carry no names themselves but may contain objects that do.
type redactingArrayMarshaler struct{ zapcore.ArrayMarshaler }

func (m redactingArrayMarshaler) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	return m.ArrayMarshaler.MarshalLogArray(redactingArrayEncoder{enc})
}

// redactingArrayEncoder embeds its delegate because every method it does not
// override appends a keyless primitive, and a value with no name cannot match
// the contract.
type redactingArrayEncoder struct{ zapcore.ArrayEncoder }

func (e redactingArrayEncoder) AppendObject(marshaler zapcore.ObjectMarshaler) error {
	return e.ArrayEncoder.AppendObject(redactingObjectMarshaler{marshaler})
}

func (e redactingArrayEncoder) AppendArray(marshaler zapcore.ArrayMarshaler) error {
	return e.ArrayEncoder.AppendArray(redactingArrayMarshaler{marshaler})
}

func (e redactingArrayEncoder) AppendReflected(value interface{}) error {
	if v, changed := redactReflected(value, 0); changed {
		return e.ArrayEncoder.AppendReflected(v)
	}
	return e.ArrayEncoder.AppendReflected(value)
}

// redactingObjectEncoder wraps a zapcore.ObjectEncoder and censors every key
// matching the contract, at any nesting depth.
//
// It deliberately does not embed zapcore.ObjectEncoder: every method of that
// interface takes a key, so if zapcore ever gains one this must fail to compile
// rather than quietly pass a new kind of key through uncensored.
type redactingObjectEncoder struct {
	enc zapcore.ObjectEncoder
	// censorAll is set once a secret-named namespace is opened. Namespaces are
	// never closed, so everything added afterwards lives inside that secret.
	censorAll bool
}

// censor writes the placeholder and reports true when the key must not carry a
// real value.
func (e *redactingObjectEncoder) censor(key string) bool {
	if e.censorAll || ShouldRedact(key) {
		e.enc.AddString(key, redactedPlaceholder)
		return true
	}
	return false
}

func (e *redactingObjectEncoder) AddArray(key string, marshaler zapcore.ArrayMarshaler) error {
	if e.censor(key) {
		return nil
	}
	return e.enc.AddArray(key, redactingArrayMarshaler{marshaler})
}

func (e *redactingObjectEncoder) AddObject(key string, marshaler zapcore.ObjectMarshaler) error {
	if e.censor(key) {
		return nil
	}
	return e.enc.AddObject(key, redactingObjectMarshaler{marshaler})
}

func (e *redactingObjectEncoder) AddReflected(key string, value interface{}) error {
	if e.censor(key) {
		return nil
	}
	if v, changed := redactReflected(value, 0); changed {
		return e.enc.AddReflected(key, v)
	}
	return e.enc.AddReflected(key, value)
}

func (e *redactingObjectEncoder) OpenNamespace(key string) {
	e.enc.OpenNamespace(key)
	if ShouldRedact(key) {
		e.censorAll = true
	}
}

func (e *redactingObjectEncoder) AddBinary(key string, value []byte) {
	if e.censor(key) {
		return
	}
	e.enc.AddBinary(key, value)
}

func (e *redactingObjectEncoder) AddByteString(key string, value []byte) {
	if e.censor(key) {
		return
	}
	e.enc.AddByteString(key, value)
}

func (e *redactingObjectEncoder) AddBool(key string, value bool) {
	if e.censor(key) {
		return
	}
	e.enc.AddBool(key, value)
}

func (e *redactingObjectEncoder) AddComplex128(key string, value complex128) {
	if e.censor(key) {
		return
	}
	e.enc.AddComplex128(key, value)
}

func (e *redactingObjectEncoder) AddComplex64(key string, value complex64) {
	if e.censor(key) {
		return
	}
	e.enc.AddComplex64(key, value)
}

func (e *redactingObjectEncoder) AddDuration(key string, value time.Duration) {
	if e.censor(key) {
		return
	}
	e.enc.AddDuration(key, value)
}

func (e *redactingObjectEncoder) AddFloat64(key string, value float64) {
	if e.censor(key) {
		return
	}
	e.enc.AddFloat64(key, value)
}

func (e *redactingObjectEncoder) AddFloat32(key string, value float32) {
	if e.censor(key) {
		return
	}
	e.enc.AddFloat32(key, value)
}

func (e *redactingObjectEncoder) AddInt(key string, value int) {
	if e.censor(key) {
		return
	}
	e.enc.AddInt(key, value)
}

func (e *redactingObjectEncoder) AddInt64(key string, value int64) {
	if e.censor(key) {
		return
	}
	e.enc.AddInt64(key, value)
}

func (e *redactingObjectEncoder) AddInt32(key string, value int32) {
	if e.censor(key) {
		return
	}
	e.enc.AddInt32(key, value)
}

func (e *redactingObjectEncoder) AddInt16(key string, value int16) {
	if e.censor(key) {
		return
	}
	e.enc.AddInt16(key, value)
}

func (e *redactingObjectEncoder) AddInt8(key string, value int8) {
	if e.censor(key) {
		return
	}
	e.enc.AddInt8(key, value)
}

func (e *redactingObjectEncoder) AddString(key, value string) {
	if e.censor(key) {
		return
	}
	e.enc.AddString(key, value)
}

func (e *redactingObjectEncoder) AddTime(key string, value time.Time) {
	if e.censor(key) {
		return
	}
	e.enc.AddTime(key, value)
}

func (e *redactingObjectEncoder) AddUint(key string, value uint) {
	if e.censor(key) {
		return
	}
	e.enc.AddUint(key, value)
}

func (e *redactingObjectEncoder) AddUint64(key string, value uint64) {
	if e.censor(key) {
		return
	}
	e.enc.AddUint64(key, value)
}

func (e *redactingObjectEncoder) AddUint32(key string, value uint32) {
	if e.censor(key) {
		return
	}
	e.enc.AddUint32(key, value)
}

func (e *redactingObjectEncoder) AddUint16(key string, value uint16) {
	if e.censor(key) {
		return
	}
	e.enc.AddUint16(key, value)
}

func (e *redactingObjectEncoder) AddUint8(key string, value uint8) {
	if e.censor(key) {
		return
	}
	e.enc.AddUint8(key, value)
}

func (e *redactingObjectEncoder) AddUintptr(key string, value uintptr) {
	if e.censor(key) {
		return
	}
	e.enc.AddUintptr(key, value)
}

// redactReflected walks a value logged with zap.Any or zap.Reflect and censors
// every secret-named map key or struct field inside it, reporting whether
// anything changed.
//
// A value with nothing to censor is reported unchanged and re-encoded as
// itself, so json tags, MarshalJSON and field order all survive the common
// case. Only a value that actually contains a secret is rebuilt as a generic
// map, which changes its rendered shape - that is the price of not leaking it,
// and it is paid only on lines that would otherwise have leaked.
func redactReflected(value interface{}, depth int) (interface{}, bool) {
	if value == nil {
		return nil, false
	}
	return redactAny(reflect.ValueOf(value), depth)
}

func redactAny(rv reflect.Value, depth int) (interface{}, bool) {
	if !rv.IsValid() || !rv.CanInterface() || depth >= maxRedactDepth {
		return nil, false
	}

	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil, false
		}
		// The indirection is dropped only when something below changed, and a
		// JSON encoder renders a pointer as its target either way.
		return redactAny(rv.Elem(), depth)

	case reflect.Map:
		if rv.IsNil() {
			return nil, false
		}
		out := make(map[string]interface{}, rv.Len())
		changed := false
		iter := rv.MapRange()
		for iter.Next() {
			key := mapKeyName(iter.Key())
			if ShouldRedact(key) {
				out[key] = redactedPlaceholder
				changed = true
				continue
			}
			out[key], changed = redactChild(iter.Value(), depth, changed)
		}
		if !changed {
			return nil, false
		}
		return out, true

	case reflect.Struct:
		rt := rv.Type()
		out := make(map[string]interface{}, rt.NumField())
		changed := false
		for i := range rt.NumField() {
			field := rt.Field(i)
			if !field.IsExported() {
				continue
			}
			name, skip := jsonFieldName(field)
			if skip {
				continue
			}
			// Both spellings are checked: a Password field tagged json:"pw" is
			// still a password.
			if ShouldRedact(name) || ShouldRedact(field.Name) {
				out[name] = redactedPlaceholder
				changed = true
				continue
			}
			out[name], changed = redactChild(rv.Field(i), depth, changed)
		}
		if !changed {
			return nil, false
		}
		return out, true

	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return nil, false
		}
		// []byte, []string and friends hold no names, so there is nothing to
		// match and no reason to walk them element by element.
		if isScalarKind(rv.Type().Elem().Kind()) {
			return nil, false
		}
		out := make([]interface{}, rv.Len())
		changed := false
		for i := range rv.Len() {
			out[i], changed = redactChild(rv.Index(i), depth, changed)
		}
		if !changed {
			return nil, false
		}
		return out, true
	}

	return nil, false
}

// redactChild returns the value to store for a child and carries the parent's
// changed flag forward.
func redactChild(child reflect.Value, depth int, changed bool) (interface{}, bool) {
	if v, c := redactAny(child, depth+1); c {
		return v, true
	}
	if !child.CanInterface() {
		return nil, changed
	}
	return child.Interface(), changed
}

// mapKeyName renders a map key as the name a JSON encoder would give it.
func mapKeyName(key reflect.Value) string {
	if key.Kind() == reflect.String {
		return key.String()
	}
	if !key.CanInterface() {
		return ""
	}
	return fmt.Sprint(key.Interface())
}

// jsonFieldName returns the name a struct field is serialised under, and
// whether it is omitted entirely.
func jsonFieldName(field reflect.StructField) (string, bool) {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return field.Name, false
	}
	if tag == "-" {
		return "", true
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return field.Name, false
	}
	return name, false
}

func isScalarKind(kind reflect.Kind) bool {
	switch kind {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128,
		reflect.String:
		return true
	}
	return false
}
