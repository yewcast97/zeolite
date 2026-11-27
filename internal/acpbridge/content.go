package acpbridge

// What either side sends the other in the same shape.

// implementation rebuilds what either side says it is: its name and its
// version.
func implementation(o object, name string) (object, bool) {
	in, ok := member(o, name)
	out := object{}
	keepText(out, in, "title")
	return out, ok && needText(out, in, "name", "version")
}

// contentBlock rebuilds one block of content, which either side sends
// the other: text, a picture, sound, the name of a resource, or a
// resource itself. uri says how a URI in it crosses, and whether it
// does: a block that is nothing without its URI does not cross without
// it, and a picture crosses without.
func contentBlock(v any, uri func(string) (string, bool)) (object, bool) {
	in, _ := v.(object)
	kind, _ := text(in, "type")
	out := object{"type": kind}
	if annotations, ok := member(in, "annotations"); ok {
		rebuilt := object{}
		if list, ok := items(annotations, "audience"); ok {
			audience := []any{}
			for _, v := range list {
				if role, ok := v.(string); ok {
					audience = append(audience, role)
				}
			}
			rebuilt["audience"] = audience
		}
		keepText(rebuilt, annotations, "lastModified")
		keepNumbers(rebuilt, annotations, "priority")
		out["annotations"] = rebuilt
	}
	crossed := func(o object) (string, bool) {
		raw, ok := text(o, "uri")
		if !ok {
			return "", false
		}
		return uri(raw)
	}
	switch kind {
	case "text":
		return out, needText(out, in, "text")
	case "image":
		if mapped, ok := crossed(in); ok {
			out["uri"] = mapped
		}
		return out, needText(out, in, "data", "mimeType")
	case "audio":
		return out, needText(out, in, "data", "mimeType")
	case "resource_link":
		mapped, ok := crossed(in)
		out["uri"] = mapped
		keepText(out, in, "title", "description", "mimeType")
		keepWhole(out, in, "size")
		return out, ok && needText(out, in, "name")
	case "resource":
		resource, _ := member(in, "resource")
		rebuilt := object{}
		mapped, ok := crossed(resource)
		rebuilt["uri"] = mapped
		keepText(rebuilt, resource, "mimeType")
		// It holds text, or what is not text as base64.
		_, isBlob := text(resource, "blob")
		held := "text"
		if isBlob {
			held = "blob"
		}
		out["resource"] = rebuilt
		return out, ok && needText(rebuilt, resource, held)
	}
	return nil, false
}
