package transcript

import (
	"encoding/json"
	"strconv"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// execMessage parses the protobuf JSON of an exec message into a canonical
// JSON value. Fields our schema knows are written the way protojson writes
// them: under their JSON names, enums by name, defaults left out. Fields it
// does not know are kept as they came, so that what only one client sends
// shows up in the comparison.
func execMessage(raw string, schema proto.Message) any {
	v := parse([]byte(raw))
	if v == nil {
		return map[string]any{"invalidJson": raw}
	}
	return canonMessage(schema.ProtoReflect().Descriptor(), v)
}

func canonMessage(md protoreflect.MessageDescriptor, v any) any {
	in, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		fd := md.Fields().ByJSONName(key)
		if fd == nil {
			fd = md.Fields().ByTextName(key)
		}
		if fd == nil {
			out[key] = value
			continue
		}
		if value == nil {
			continue // null means the default
		}
		c := canonField(fd, value)
		if !fd.HasPresence() && isDefault(fd, c) {
			continue
		}
		out[fd.JSONName()] = c
	}
	return out
}

func canonField(fd protoreflect.FieldDescriptor, v any) any {
	switch {
	case fd.IsMap():
		in, ok := v.(map[string]any)
		if !ok {
			return v
		}
		out := make(map[string]any, len(in))
		for k, e := range in {
			out[k] = canonValue(fd.MapValue(), e)
		}
		return out
	case fd.IsList():
		in, ok := v.([]any)
		if !ok {
			return v
		}
		out := make([]any, len(in))
		for i, e := range in {
			out[i] = canonValue(fd, e)
		}
		return out
	}
	return canonValue(fd, v)
}

func canonValue(fd protoreflect.FieldDescriptor, v any) any {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return canonMessage(fd.Message(), v)
	case protoreflect.EnumKind:
		if n, ok := v.(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				if ev := fd.Enum().Values().ByNumber(protoreflect.EnumNumber(i)); ev != nil { //nolint:gosec // an enum number
					return string(ev.Name())
				}
			}
		}
		return v
	case protoreflect.BoolKind, protoreflect.StringKind, protoreflect.BytesKind:
		return v
	}
	// Numbers: protobuf JSON writes 64-bit integers as strings and allows
	// strings for the others.
	if s, ok := v.(string); ok {
		if _, err := strconv.ParseFloat(s, 64); err == nil {
			return json.Number(s)
		}
	}
	return v
}

// isDefault reports whether v is the value protobuf JSON leaves out for a
// field without presence.
func isDefault(fd protoreflect.FieldDescriptor, v any) bool {
	switch x := v.(type) {
	case bool:
		return !x
	case string:
		if fd.Kind() == protoreflect.EnumKind {
			zero := fd.Enum().Values().ByNumber(0)
			return zero != nil && x == string(zero.Name())
		}
		return x == ""
	case json.Number:
		f, err := x.Float64()
		return err == nil && f == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}
