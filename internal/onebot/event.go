package onebot

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Sender contains the common OneBot 11 sender fields.
type Sender struct {
	UserID   int64  `json:"user_id,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Card     string `json:"card,omitempty"`
	Sex      string `json:"sex,omitempty"`
	Age      int    `json:"age,omitempty"`
	Area     string `json:"area,omitempty"`
	Level    string `json:"level,omitempty"`
	Role     string `json:"role,omitempty"`
	Title    string `json:"title,omitempty"`
}

// Anonymous describes an anonymous group sender.
type Anonymous struct {
	ID   int64  `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Flag string `json:"flag,omitempty"`
}

// File is used by group upload and offline-file notice events.
type File struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Size  int64  `json:"size,omitempty"`
	BusID int64  `json:"busid,omitempty"`
	URL   string `json:"url,omitempty"`
}

// Event covers OneBot 11 message, message_sent, request, notice, and meta_event
// payloads. Raw always contains an independent copy of the original JSON.
type Event struct {
	Time     int64  `json:"time"`
	SelfID   int64  `json:"self_id"`
	PostType string `json:"post_type"`

	MessageType string     `json:"message_type,omitempty"`
	SubType     string     `json:"sub_type,omitempty"`
	MessageID   int64      `json:"message_id,omitempty"`
	MessageSeq  int64      `json:"message_seq,omitempty"`
	UserID      int64      `json:"user_id,omitempty"`
	GroupID     int64      `json:"group_id,omitempty"`
	Font        int        `json:"font,omitempty"`
	Message     Message    `json:"message,omitempty"`
	RawMessage  string     `json:"raw_message,omitempty"`
	Sender      Sender     `json:"sender,omitempty"`
	Anonymous   *Anonymous `json:"anonymous,omitempty"`

	RequestType string `json:"request_type,omitempty"`
	Comment     string `json:"comment,omitempty"`
	Flag        string `json:"flag,omitempty"`

	NoticeType string `json:"notice_type,omitempty"`
	OperatorID int64  `json:"operator_id,omitempty"`
	Duration   int64  `json:"duration,omitempty"`
	File       *File  `json:"file,omitempty"`
	CardNew    string `json:"card_new,omitempty"`
	CardOld    string `json:"card_old,omitempty"`
	HonorType  string `json:"honor_type,omitempty"`

	MetaEventType string `json:"meta_event_type,omitempty"`
	Status        any    `json:"status,omitempty"`
	Interval      int64  `json:"interval,omitempty"`

	Raw json.RawMessage `json:"-"`
}

// Target identifies a private user or a group. GroupID takes precedence in Send.
type Target struct {
	UserID  int64 `json:"userId"`
	GroupID int64 `json:"groupId"`
}

// Target returns the destination represented by a message event.
func (e Event) Target() Target {
	if e.MessageType == "group" {
		return Target{GroupID: e.GroupID}
	}
	return Target{UserID: e.UserID}
}

// ParseEvent decodes an event while preserving its original payload and both
// OneBot message encodings.
func ParseEvent(raw []byte) (Event, error) {
	var event Event
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return event, fmt.Errorf("onebot: event must be a JSON object")
	}
	type eventAlias Event
	if err := json.Unmarshal(trimmed, (*eventAlias)(&event)); err != nil {
		return Event{}, fmt.Errorf("onebot: decode event: %w", err)
	}
	if event.PostType == "" {
		return Event{}, fmt.Errorf("onebot: event post_type is required")
	}
	event.Raw = append(json.RawMessage(nil), raw...)
	return event, nil
}
