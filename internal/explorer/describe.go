package explorer

import (
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ServiceInfo describes a gRPC service and its methods.
type ServiceInfo struct {
	Name    string       `json:"name"`
	Methods []MethodInfo `json:"methods"`
}

// MethodInfo describes one RPC method.
type MethodInfo struct {
	Name            string `json:"name"`
	FullName        string `json:"full_name"`
	InputType       string `json:"input_type"`
	OutputType      string `json:"output_type"`
	ClientStreaming bool   `json:"client_streaming"`
	ServerStreaming bool   `json:"server_streaming"`
	// Callable is true for unary methods, the only kind call_method supports.
	Callable bool `json:"callable"`
}

// MessageInfo describes a protobuf message and includes an example JSON value
// that can be used as a starting point for a request body.
type MessageInfo struct {
	Name    string      `json:"name"`
	Fields  []FieldInfo `json:"fields"`
	Example any         `json:"example"`
}

// FieldInfo describes one field of a message.
type FieldInfo struct {
	Name     string `json:"name"`
	JSONName string `json:"json_name"`
	Number   int32  `json:"number"`
	// Type is the protobuf kind: string, int64, bool, enum, message, ...
	Type string `json:"type"`
	// TypeName is the fully-qualified message or enum name, when applicable.
	TypeName string `json:"type_name,omitempty"`
	// Label is "repeated", "map", "optional" (explicit presence) or "singular".
	Label      string   `json:"label"`
	OneOf      string   `json:"oneof,omitempty"`
	MapKey     string   `json:"map_key,omitempty"`
	MapValue   string   `json:"map_value,omitempty"`
	EnumValues []string `json:"enum_values,omitempty"`
}

func describeService(sd protoreflect.ServiceDescriptor) ServiceInfo {
	info := ServiceInfo{Name: string(sd.FullName()), Methods: []MethodInfo{}}
	methods := sd.Methods()
	for i := 0; i < methods.Len(); i++ {
		info.Methods = append(info.Methods, describeMethod(methods.Get(i)))
	}
	return info
}

func describeMethod(md protoreflect.MethodDescriptor) MethodInfo {
	return MethodInfo{
		Name:            string(md.Name()),
		FullName:        string(md.Parent().FullName()) + "/" + string(md.Name()),
		InputType:       string(md.Input().FullName()),
		OutputType:      string(md.Output().FullName()),
		ClientStreaming: md.IsStreamingClient(),
		ServerStreaming: md.IsStreamingServer(),
		Callable:        !md.IsStreamingClient() && !md.IsStreamingServer(),
	}
}

func describeMessage(msg protoreflect.MessageDescriptor) MessageInfo {
	info := MessageInfo{
		Name:    string(msg.FullName()),
		Fields:  []FieldInfo{},
		Example: exampleMessage(msg, map[protoreflect.FullName]bool{}, 0),
	}
	fields := msg.Fields()
	for i := 0; i < fields.Len(); i++ {
		info.Fields = append(info.Fields, describeField(fields.Get(i)))
	}
	return info
}

func describeField(fd protoreflect.FieldDescriptor) FieldInfo {
	info := FieldInfo{
		Name:     string(fd.Name()),
		JSONName: fd.JSONName(),
		Number:   int32(fd.Number()),
		Type:     fd.Kind().String(),
		TypeName: typeName(fd),
		Label:    "singular",
	}
	switch {
	case fd.IsMap():
		info.Label = "map"
		info.Type = "map"
		info.TypeName = ""
		info.MapKey = fd.MapKey().Kind().String()
		info.MapValue = valueTypeName(fd.MapValue())
	case fd.IsList():
		info.Label = "repeated"
	case fd.HasPresence() && fd.ContainingOneof() == nil && fd.Message() == nil:
		info.Label = "optional"
	}
	if oneof := fd.ContainingOneof(); oneof != nil && !oneof.IsSynthetic() {
		info.OneOf = string(oneof.Name())
	}
	if ed := fd.Enum(); ed != nil && !fd.IsMap() {
		values := ed.Values()
		for i := 0; i < values.Len(); i++ {
			info.EnumValues = append(info.EnumValues, string(values.Get(i).Name()))
		}
	}
	return info
}

func typeName(fd protoreflect.FieldDescriptor) string {
	switch {
	case fd.Message() != nil:
		return string(fd.Message().FullName())
	case fd.Enum() != nil:
		return string(fd.Enum().FullName())
	}
	return ""
}

func valueTypeName(fd protoreflect.FieldDescriptor) string {
	if name := typeName(fd); name != "" {
		return name
	}
	return fd.Kind().String()
}

// maxExampleDepth bounds example generation for deeply nested messages.
const maxExampleDepth = 4

// exampleMessage builds a JSON-compatible value (in protojson form) that shows
// the shape of msg. Recursive message types are cut off when they repeat, and
// only the first field of each oneof is included because protojson rejects
// requests that set more than one.
func exampleMessage(msg protoreflect.MessageDescriptor, visiting map[protoreflect.FullName]bool, depth int) any {
	if v, ok := wellKnownExample(msg.FullName()); ok {
		return v
	}
	out := map[string]any{}
	if depth >= maxExampleDepth || visiting[msg.FullName()] {
		return out
	}
	visiting[msg.FullName()] = true
	defer delete(visiting, msg.FullName())

	seenOneofs := map[protoreflect.FullName]bool{}
	fields := msg.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if oneof := fd.ContainingOneof(); oneof != nil && !oneof.IsSynthetic() {
			if seenOneofs[oneof.FullName()] {
				continue
			}
			seenOneofs[oneof.FullName()] = true
		}
		switch {
		case fd.IsMap():
			out[fd.JSONName()] = map[string]any{
				mapKeyExample(fd.MapKey()): exampleValue(fd.MapValue(), visiting, depth),
			}
		case fd.IsList():
			out[fd.JSONName()] = []any{exampleValue(fd, visiting, depth)}
		default:
			out[fd.JSONName()] = exampleValue(fd, visiting, depth)
		}
	}
	return out
}

func exampleValue(fd protoreflect.FieldDescriptor, visiting map[protoreflect.FullName]bool, depth int) any {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return exampleMessage(fd.Message(), visiting, depth+1)
	case protoreflect.EnumKind:
		values := fd.Enum().Values()
		// Prefer the first non-zero value: zero is usually *_UNSPECIFIED.
		if values.Len() > 1 {
			return string(values.Get(1).Name())
		}
		return string(values.Get(0).Name())
	case protoreflect.BoolKind:
		return false
	case protoreflect.StringKind:
		return ""
	case protoreflect.BytesKind:
		return "" // base64 in protojson
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return 0.0
	default: // all integer kinds
		return 0
	}
}

func mapKeyExample(fd protoreflect.FieldDescriptor) string {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return "key"
	case protoreflect.BoolKind:
		return "true"
	default:
		return "0"
	}
}

func wellKnownExample(name protoreflect.FullName) (any, bool) {
	switch name {
	case "google.protobuf.Timestamp":
		return "2026-01-01T00:00:00Z", true
	case "google.protobuf.Duration":
		return "1.5s", true
	case "google.protobuf.FieldMask":
		return "fieldOne,fieldTwo", true
	case "google.protobuf.Struct":
		return map[string]any{}, true
	case "google.protobuf.Value":
		return nil, true
	case "google.protobuf.ListValue":
		return []any{}, true
	case "google.protobuf.Empty":
		return map[string]any{}, true
	case "google.protobuf.Any":
		return map[string]any{}, true // a real value needs "@type" plus the message fields
	case "google.protobuf.StringValue", "google.protobuf.BytesValue":
		return "", true
	case "google.protobuf.BoolValue":
		return false, true
	case "google.protobuf.Int32Value", "google.protobuf.Int64Value",
		"google.protobuf.UInt32Value", "google.protobuf.UInt64Value":
		return 0, true
	case "google.protobuf.FloatValue", "google.protobuf.DoubleValue":
		return 0.0, true
	}
	return nil, false
}
