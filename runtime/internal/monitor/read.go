package monitor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const maxRead = 16 << 20
const maxLine = 8 << 20
const maxDecoded = 32 << 20

type unit struct {
	End   int64
	Lines [][]byte
}

// Seeded forks can contain end-seed markers inherited from ancestors. Find the
// last tagged marker across the captured baseline before reducing any events.
// A header-only or partially published SDK session remains unconfirmed.
func prepareSeed(src *Source, f *os.File, size int64) error {
	var offset int64
	seeded, found, first := false, false, true
	var cut int64
	for offset < size {
		units, err := readUnits(f, offset, size)
		if err != nil {
			return err
		}
		if len(units) == 0 {
			if seeded {
				return errors.New("seed_boundary_unconfirmed")
			}
			return nil
		}
		for _, u := range units {
			for _, line := range u.Lines {
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				o := obj(line)
				if first {
					first = false
					seeded = str(o, "type") == "session" && boolean(o, "isSeeded")
					if !seeded {
						return nil
					}
				}
				if str(o, "type") == "session/end-seed" && boolean(obj(o["data"]), "inherited") {
					var seq int64
					if json.Unmarshal(o["seq"], &seq) != nil || seq < 0 {
						return errors.New("invalid_seed_boundary")
					}
					found, cut = true, seq
				}
			}
			offset = u.End
		}
	}
	if seeded {
		if !found {
			return errors.New("seed_boundary_unconfirmed")
		}
		src.Parser.SeedReady, src.Parser.SeedCut = true, cut
	}
	return nil
}

func readUnits(f *os.File, offset, size int64) ([]unit, error) {
	n := size - offset
	if n > maxRead {
		n = maxRead
	}
	if n <= 0 {
		return nil, nil
	}
	b := make([]byte, int(n))
	nr, err := f.ReadAt(b, offset)
	if err != nil && err != io.EOF {
		return nil, errors.New("read_failed")
	}
	b = b[:nr]
	units := []unit{}
	if !strings.HasSuffix(f.Name(), ".zstd") {
		pos := 0
		for {
			i := bytes.IndexByte(b[pos:], '\n')
			if i < 0 {
				if len(b)-pos > maxLine {
					return units, errors.New("line_limit_exceeded")
				}
				break
			}
			i += pos
			if i-pos > maxLine {
				return units, errors.New("line_limit_exceeded")
			}
			line := bytes.TrimSpace(b[pos:i])
			pos = i + 1
			units = append(units, unit{offset + int64(pos), [][]byte{line}})
		}
		return units, nil
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(maxDecoded))
	if err != nil {
		return nil, errors.New("zstd_init_failed")
	}
	defer decoder.Close()
	for pos := 0; pos < len(b); {
		n, err := frameSize(b[pos:])
		if errors.Is(err, io.ErrUnexpectedEOF) {
			if len(b)-pos >= maxRead {
				return units, errors.New("frame_limit_exceeded")
			}
			break
		}
		if err != nil {
			return units, err
		}
		decoded, err := decoder.DecodeAll(b[pos:pos+n], nil)
		if err != nil {
			return units, errors.New("invalid_zstd_frame")
		}
		if len(decoded) > maxDecoded {
			return units, errors.New("decoded_frame_limit_exceeded")
		}
		if len(decoded) > 0 && decoded[len(decoded)-1] != '\n' {
			return units, errors.New("incomplete_frame_jsonl")
		}
		lines := bytes.Split(decoded, []byte{'\n'})
		for _, line := range lines {
			if len(line) > maxLine {
				return units, errors.New("line_limit_exceeded")
			}
		}
		pos += n
		units = append(units, unit{offset + int64(pos), lines})
	}
	return units, nil
}

// Locate exactly one standard Zstd frame; DecodeAll subsequently validates its
// blocks and checksum. No partial frame advances the durable cursor.
func frameSize(b []byte) (int, error) {
	var h zstd.Header
	if err := h.Decode(b); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, errors.New("invalid_zstd_header")
	}
	if h.Skippable || !h.HasCheckSum {
		return 0, errors.New("unsupported_zstd_frame")
	}
	if h.HasFCS && h.FrameContentSize > maxDecoded {
		return 0, errors.New("decoded_frame_limit_exceeded")
	}
	pos := h.HeaderSize
	for {
		if pos+3 > len(b) {
			return 0, io.ErrUnexpectedEOF
		}
		block := uint32(b[pos]) | uint32(b[pos+1])<<8 | uint32(b[pos+2])<<16
		pos += 3
		typ := (block >> 1) & 3
		n := int(block >> 3)
		if typ == 3 {
			return 0, errors.New("invalid_zstd_block")
		}
		if typ == 1 {
			n = 1
		}
		pos += n
		if pos > maxRead {
			return 0, errors.New("frame_limit_exceeded")
		}
		if pos > len(b) {
			return 0, io.ErrUnexpectedEOF
		}
		if block&1 != 0 {
			pos += 4
			if pos > len(b) {
				return 0, io.ErrUnexpectedEOF
			}
			return pos, nil
		}
	}
}

func fingerprint(f *os.File, start, length int64) string {
	if length <= 0 {
		return ""
	}
	b := make([]byte, length)
	n, err := f.ReadAt(b, start)
	if err != nil && err != io.EOF {
		return "read_error"
	}
	h := sha256.Sum256(b[:n])
	return hex.EncodeToString(h[:])
}

func anchors(f *os.File, offset int64) (string, string) {
	n := min(offset, int64(256))
	return fingerprint(f, 0, n), fingerprint(f, offset-n, n)
}
