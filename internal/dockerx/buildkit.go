package dockerx

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
)

// buildKitTraceID marks a build-stream message whose aux payload is one of
// BuildKit's progress updates rather than a line of output.
const buildKitTraceID = "moby.buildkit.trace"

// buildKitProgress turns BuildKit's progress stream into the plain lines the
// operation log shows.
//
// BuildKit reports progress as base64-encoded protobuf StatusResponse
// messages, not as the text lines the legacy builder streams. Each update
// repeats the steps it touches, so a step is announced once — when it starts or
// is found in the cache — and its output is reassembled into whole lines.
// Only the handful of fields the log needs are decoded; everything else is
// skipped, so a newer daemon adding fields cannot break the parse.
type buildKitProgress struct {
	log       func(string)
	announced map[string]bool
	failed    map[string]bool
	partial   map[string]string
	order     []string
}

func newBuildKitProgress(log func(string)) *buildKitProgress {
	return &buildKitProgress{log: log, announced: map[string]bool{}, failed: map[string]bool{}, partial: map[string]string{}}
}

// handle consumes one trace message. A payload that cannot be decoded is
// dropped: progress is commentary, and the build's own result is what decides
// the release.
func (p *buildKitProgress) handle(aux json.RawMessage) {
	var payload []byte
	if json.Unmarshal(aux, &payload) != nil {
		return
	}
	_ = protoFields(payload, func(field int, value []byte, _ uint64) error {
		switch field {
		case 1:
			return p.vertex(value)
		case 3:
			return p.vertexLog(value)
		case 4:
			return p.warning(value)
		}
		return nil
	})
}

func (p *buildKitProgress) vertex(data []byte) error {
	var digest, name, failure string
	var cached, started bool
	err := protoFields(data, func(field int, value []byte, number uint64) error {
		switch field {
		case 1:
			digest = string(value)
		case 3:
			name = string(value)
		case 4:
			cached = number != 0
		case 5:
			started = true
		case 7:
			failure = string(value)
		}
		return nil
	})
	if err != nil || digest == "" {
		return err
	}
	if !p.announced[digest] && (cached || started) {
		p.announced[digest] = true
		if cached {
			p.log("CACHED " + name)
		} else {
			p.log(name)
		}
	}
	if failure != "" && !p.failed[digest] {
		p.failed[digest] = true
		p.log("ERROR " + name + ": " + failure)
	}
	return nil
}

func (p *buildKitProgress) vertexLog(data []byte) error {
	var digest string
	var message []byte
	err := protoFields(data, func(field int, value []byte, _ uint64) error {
		switch field {
		case 1:
			digest = string(value)
		case 4:
			message = value
		}
		return nil
	})
	if err != nil || len(message) == 0 {
		return err
	}
	if _, seen := p.partial[digest]; !seen {
		p.order = append(p.order, digest)
	}
	text := p.partial[digest] + strings.ReplaceAll(string(message), "\r", "\n")
	lines := strings.Split(text, "\n")
	p.partial[digest] = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		if strings.TrimSpace(line) != "" {
			p.log(line)
		}
	}
	return nil
}

func (p *buildKitProgress) warning(data []byte) error {
	return protoFields(data, func(field int, value []byte, _ uint64) error {
		if field == 3 && len(value) > 0 {
			p.log("WARNING: " + string(value))
		}
		return nil
	})
}

// flush writes the output a step left without a final newline.
func (p *buildKitProgress) flush() {
	for _, digest := range p.order {
		if line := p.partial[digest]; strings.TrimSpace(line) != "" {
			p.log(line)
		}
		p.partial[digest] = ""
	}
}

var errMalformedProto = errors.New("malformed protobuf message")

// protoFields walks the top level of one protobuf message, handing visit each
// field's number with either its length-delimited bytes or its varint value.
// Fixed-width fields are skipped; no field the progress log reads uses them.
func protoFields(data []byte, visit func(field int, value []byte, number uint64) error) error {
	for len(data) > 0 {
		key, n := binary.Uvarint(data)
		if n <= 0 {
			return errMalformedProto
		}
		data = data[n:]
		field := int(key >> 3)
		switch key & 7 {
		case 0:
			number, n := binary.Uvarint(data)
			if n <= 0 {
				return errMalformedProto
			}
			data = data[n:]
			if err := visit(field, nil, number); err != nil {
				return err
			}
		case 1:
			if len(data) < 8 {
				return errMalformedProto
			}
			data = data[8:]
		case 2:
			length, n := binary.Uvarint(data)
			if n <= 0 || length > uint64(len(data)-n) {
				return errMalformedProto
			}
			value := data[n : n+int(length)]
			data = data[n+int(length):]
			if err := visit(field, value, 0); err != nil {
				return err
			}
		case 5:
			if len(data) < 4 {
				return errMalformedProto
			}
			data = data[4:]
		default:
			return errMalformedProto
		}
	}
	return nil
}
