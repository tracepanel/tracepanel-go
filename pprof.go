package tracepanel

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
)

// What runtime/pprof writes, read back: a CPU profile is a gzipped profile.proto message. Only what a trace's
// profile needs is read (its samples with their labels, its locations and functions, its period), with a decoder of
// its own, so the SDK keeps no dependencies.

// cpuSample is one sample of the profile: its stack, innermost frame first, how many times it was seen, and the
// trace label it carried ("" for none).
type cpuSample struct {
	stack []cpuFrame
	count int64
	trace string
}

// cpuFrame is a function in a stack, where it was running (an inlined call is a frame of its own).
type cpuFrame struct {
	function, file string
	line, start    int
}

type cpuProfile struct {
	samples []cpuSample
	// How often a sample is taken, in nanoseconds of CPU time.
	periodNs int64
}

type pbLine struct{ function, line uint64 }

type pbFunction struct{ name, file, start uint64 }

type pbSample struct {
	locations []uint64
	values    []int64
	labels    [][2]uint64
}

var errProfile = errors.New("tracepanel: a CPU profile it can't read")

// parseCPUProfile reads a CPU profile as runtime/pprof wrote it, the samples carrying label `key` named by its value.
func parseCPUProfile(data []byte, key string) (*cpuProfile, error) {
	if len(data) == 0 {
		return &cpuProfile{}, nil
	}
	if bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if data, err = io.ReadAll(reader); err != nil {
			return nil, err
		}
	}
	var (
		strings   []string
		samples   []pbSample
		locations = map[uint64][]pbLine{}
		functions = map[uint64]pbFunction{}
		cpuIndex  = -1
		types     int
		period    int64
	)
	err := fields(data, func(field int, wire int, value uint64, raw []byte) error {
		switch field {
		case 1: // sample_type: the second (cpu, nanoseconds) is the one with time; the first counts samples.
			types++
		case 2:
			sample, err := parseSample(raw)
			if err != nil {
				return err
			}
			samples = append(samples, sample)
		case 4:
			id, lines, err := parseLocation(raw)
			if err != nil {
				return err
			}
			locations[id] = lines
		case 5:
			id, function, err := parseFunction(raw)
			if err != nil {
				return err
			}
			functions[id] = function
		case 6:
			strings = append(strings, string(raw))
		case 12:
			period = int64(value)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if types > 0 {
		cpuIndex = 0
	}
	text := func(index uint64) string {
		if index < uint64(len(strings)) {
			return strings[index]
		}
		return ""
	}
	profile := &cpuProfile{periodNs: period}
	for _, sample := range samples {
		count := int64(1)
		if cpuIndex >= 0 && cpuIndex < len(sample.values) {
			count = sample.values[cpuIndex]
		}
		if count <= 0 {
			continue
		}
		parsed := cpuSample{count: count}
		for _, label := range sample.labels {
			if text(label[0]) == key {
				parsed.trace = text(label[1])
			}
		}
		for _, id := range sample.locations {
			for _, line := range locations[id] {
				function := functions[line.function]
				parsed.stack = append(parsed.stack, cpuFrame{
					function: text(function.name), file: text(function.file), line: int(line.line), start: int(function.start),
				})
			}
		}
		profile.samples = append(profile.samples, parsed)
	}
	return profile, nil
}

func parseSample(data []byte) (pbSample, error) {
	var sample pbSample
	err := fields(data, func(field int, wire int, value uint64, raw []byte) error {
		switch field {
		case 1:
			return varints(wire, value, raw, func(v uint64) { sample.locations = append(sample.locations, v) })
		case 2:
			return varints(wire, value, raw, func(v uint64) { sample.values = append(sample.values, int64(v)) })
		case 3:
			var label [2]uint64
			err := fields(raw, func(field int, _ int, value uint64, _ []byte) error {
				if field == 1 || field == 2 {
					label[field-1] = value
				}
				return nil
			})
			sample.labels = append(sample.labels, label)
			return err
		}
		return nil
	})
	return sample, err
}

func parseLocation(data []byte) (uint64, []pbLine, error) {
	var id uint64
	var lines []pbLine
	err := fields(data, func(field int, _ int, value uint64, raw []byte) error {
		switch field {
		case 1:
			id = value
		case 4:
			var line pbLine
			err := fields(raw, func(field int, _ int, value uint64, _ []byte) error {
				switch field {
				case 1:
					line.function = value
				case 2:
					line.line = value
				}
				return nil
			})
			lines = append(lines, line)
			return err
		}
		return nil
	})
	return id, lines, err
}

func parseFunction(data []byte) (uint64, pbFunction, error) {
	var id uint64
	var function pbFunction
	err := fields(data, func(field int, _ int, value uint64, _ []byte) error {
		switch field {
		case 1:
			id = value
		case 2:
			function.name = value
		case 4:
			function.file = value
		case 5:
			function.start = value
		}
		return nil
	})
	return id, function, err
}

// varints is a repeated integer field, packed (one length-delimited run) or not.
func varints(wire int, value uint64, raw []byte, each func(uint64)) error {
	if wire == 0 {
		each(value)
		return nil
	}
	for len(raw) > 0 {
		v, n := uvarint(raw)
		if n <= 0 {
			return errProfile
		}
		each(v)
		raw = raw[n:]
	}
	return nil
}

// fields calls fn with each field of a protobuf message: a varint's value, or a length-delimited field's bytes.
func fields(data []byte, fn func(field int, wire int, value uint64, raw []byte) error) error {
	for len(data) > 0 {
		tag, n := uvarint(data)
		if n <= 0 {
			return errProfile
		}
		data = data[n:]
		field, wire := int(tag>>3), int(tag&7)
		var value uint64
		var raw []byte
		switch wire {
		case 0:
			if value, n = uvarint(data); n <= 0 {
				return errProfile
			}
			data = data[n:]
		case 1:
			if len(data) < 8 {
				return errProfile
			}
			data = data[8:]
		case 2:
			length, n := uvarint(data)
			if n <= 0 || uint64(len(data)-n) < length {
				return errProfile
			}
			raw = data[n : n+int(length)]
			data = data[n+int(length):]
		case 5:
			if len(data) < 4 {
				return errProfile
			}
			data = data[4:]
		default:
			return errProfile
		}
		if err := fn(field, wire, value, raw); err != nil {
			return err
		}
	}
	return nil
}

func uvarint(data []byte) (uint64, int) {
	var value uint64
	for i, b := range data {
		if i == 10 {
			return 0, -1
		}
		value |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			return value, i + 1
		}
	}
	return 0, 0
}
