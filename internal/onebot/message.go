package onebot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Segment is a OneBot message segment. Unknown segment types and their data are
// preserved when decoding and encoding.
type Segment struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// Message is the array representation of a OneBot message.
type Message []Segment

var (
	textEscaper = strings.NewReplacer(
		"&", "&amp;",
		"[", "&#91;",
		"]", "&#93;",
	)
	textUnescaper = strings.NewReplacer(
		"&amp;", "&",
		"&#91;", "[",
		"&#93;", "]",
		"&#44;", ",",
	)
	valueEscaper = strings.NewReplacer(
		"&", "&amp;",
		"[", "&#91;",
		"]", "&#93;",
		",", "&#44;",
	)
)

// EscapeText escapes plain text for inclusion in a CQ-code string.
func EscapeText(s string) string { return textEscaper.Replace(s) }

// UnescapeText decodes OneBot CQ entities in text or parameter values.
func UnescapeText(s string) string { return textUnescaper.Replace(s) }

// EscapeCQValue additionally escapes commas, which delimit CQ parameters.
func EscapeCQValue(s string) string { return valueEscaper.Replace(s) }

// UnescapeCQValue decodes OneBot CQ entities in a parameter value.
func UnescapeCQValue(s string) string { return textUnescaper.Replace(s) }

// EncodeCQ converts an array message to its CQ-code representation. Parameter
// keys are sorted to make the result deterministic.
func EncodeCQ(message Message) string {
	var b strings.Builder
	for _, segment := range message {
		if segment.Type == "text" {
			b.WriteString(EscapeText(stringValue(segment.Data["text"])))
			continue
		}
		b.WriteString("[CQ:")
		b.WriteString(EscapeCQValue(segment.Type))
		keys := make([]string, 0, len(segment.Data))
		for key := range segment.Data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			b.WriteByte(',')
			b.WriteString(EscapeCQValue(key))
			b.WriteByte('=')
			b.WriteString(EscapeCQValue(stringValue(segment.Data[key])))
		}
		b.WriteByte(']')
	}
	return b.String()
}

// String returns the CQ-code representation of m.
func (m Message) String() string { return EncodeCQ(m) }

// DecodeCQ parses mixed plain text and CQ codes. A syntactically malformed CQ
// code is retained as text rather than silently discarded.
func DecodeCQ(raw string) Message {
	if raw == "" {
		return Message{}
	}
	message := make(Message, 0, 4)
	var text strings.Builder
	flushText := func() {
		if text.Len() == 0 {
			return
		}
		message = append(message, Segment{Type: "text", Data: map[string]any{
			"text": UnescapeText(text.String()),
		}})
		text.Reset()
	}

	for i := 0; i < len(raw); {
		if raw[i] != '[' {
			text.WriteByte(raw[i])
			i++
			continue
		}
		end := strings.IndexByte(raw[i+1:], ']')
		if end < 0 {
			text.WriteString(raw[i:])
			break
		}
		end += i + 1
		segment, ok := parseCQCode(raw[i : end+1])
		if !ok {
			text.WriteString(raw[i : end+1])
			i = end + 1
			continue
		}
		flushText()
		message = append(message, segment)
		i = end + 1
	}
	flushText()
	return message
}

func parseCQCode(raw string) (Segment, bool) {
	if len(raw) < 6 || !strings.HasPrefix(raw, "[CQ:") || raw[len(raw)-1] != ']' {
		return Segment{}, false
	}
	body := raw[4 : len(raw)-1]
	parts := strings.Split(body, ",")
	if parts[0] == "" {
		return Segment{}, false
	}
	segment := Segment{Type: UnescapeCQValue(parts[0]), Data: map[string]any{}}
	if segment.Type == "" || strings.ContainsAny(segment.Type, "[]") {
		return Segment{}, false
	}
	for _, part := range parts[1:] {
		key, value, ok := strings.Cut(part, "=")
		key = UnescapeCQValue(key)
		if !ok || key == "" || strings.ContainsAny(key, "[]") {
			return Segment{}, false
		}
		segment.Data[key] = UnescapeCQValue(value)
	}
	return segment, true
}

// ParseMessage accepts either an array message, a JSON string containing CQ
// codes, or the legacy raw_message fallback. Valid raw JSON takes precedence.
// Invalid input is retained as plain text so malformed events do not lose user
// content.
func ParseMessage(raw json.RawMessage, rawMessage string) Message {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		switch trimmed[0] {
		case '[':
			var segments []Segment
			if json.Unmarshal(trimmed, &segments) == nil {
				if segments == nil {
					return Message{}
				}
				return Message(segments)
			}
		case '"':
			var value string
			if json.Unmarshal(trimmed, &value) == nil {
				return DecodeCQ(value)
			}
		}
	}
	if rawMessage != "" {
		return DecodeCQ(rawMessage)
	}
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		return DecodeCQ(string(trimmed))
	}
	return Message{}
}

// UnmarshalJSON supports both array and CQ-string message representations.
func (m *Message) UnmarshalJSON(data []byte) error {
	if m == nil {
		return fmt.Errorf("onebot: cannot unmarshal message into nil receiver")
	}
	*m = ParseMessage(data, "")
	return nil
}

// Text returns the concatenated text segments without CQ markup.
func (m Message) Text() string {
	var b strings.Builder
	for _, segment := range m {
		if segment.Type == "text" {
			b.WriteString(stringValue(segment.Data["text"]))
		}
	}
	return b.String()
}

// At reports whether the message explicitly mentions selfID.
func (m Message) At(selfID int64) bool {
	for _, segment := range m {
		if segment.Type != "at" {
			continue
		}
		if id, ok := int64Value(segment.Data["qq"]); ok && id == selfID {
			return true
		}
		if id, ok := int64Value(segment.Data["user_id"]); ok && id == selfID {
			return true
		}
	}
	return false
}

// WithoutAt returns a copy with mentions of selfID removed. Other segments and
// their original data maps are left untouched.
func (m Message) WithoutAt(selfID int64) Message {
	result := make(Message, 0, len(m))
	for _, segment := range m {
		if segment.Type == "at" && (sameID(segment.Data["qq"], selfID) || sameID(segment.Data["user_id"], selfID)) {
			continue
		}
		result = append(result, segment)
	}
	return result
}

// Images returns all image segments in their original order.
func (m Message) Images() []Segment {
	images := make([]Segment, 0)
	for _, segment := range m {
		if segment.Type == "image" {
			images = append(images, segment)
		}
	}
	return images
}

func sameID(value any, id int64) bool {
	parsed, ok := int64Value(value)
	return ok && parsed == id
}

func int64Value(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		if uint64(v) <= uint64(^uint64(0)>>1) {
			return int64(v), true
		}
	case uint8:
		return int64(v), true
	case uint16:
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		if v <= uint64(^uint64(0)>>1) {
			return int64(v), true
		}
	case float64:
		if v >= -1<<63 && v < 1<<63 && v == float64(int64(v)) {
			return int64(v), true
		}
	case float32:
		if v >= -1<<63 && v < 1<<63 && v == float32(int64(v)) {
			return int64(v), true
		}
	case json.Number:
		id, err := v.Int64()
		return id, err == nil
	case string:
		id, err := strconv.ParseInt(v, 10, 64)
		return id, err == nil
	}
	return 0, false
}

func stringValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'g', -1, 32)
	case int:
		return strconv.Itoa(v)
	case int8:
		return strconv.FormatInt(int64(v), 10)
	case int16:
		return strconv.FormatInt(int64(v), 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint:
		return strconv.FormatUint(uint64(v), 10)
	case uint8:
		return strconv.FormatUint(uint64(v), 10)
	case uint16:
		return strconv.FormatUint(uint64(v), 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(encoded)
	}
}
