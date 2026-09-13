// Package kiroeventstream provides a minimal AWS EventStream binary protocol
// decoder, sufficient for parsing CodeWhisperer / Kiro `generateAssistantResponse`
// streaming responses.
//
// Frame layout:
//
//	totalLen(4) + headersLen(4) + preludeCrc(4) + headers + payload + messageCrc(4)
//
// Header value type 7 (string) is the only type emitted by Kiro upstream;
// other types are tolerated but skipped.
package kiroeventstream

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// Message is a decoded AWS EventStream frame.
type Message struct {
	Headers map[string]string
	Payload []byte
}

const minMessageLen = 16

// Decode pulls one message from buf. Returns:
//
//	(*Message, n, nil) — decoded one message of n bytes
//	(nil, 0, nil)      — buffer too small, need more data
//	(nil, 0, error)    — corrupted frame
func Decode(buf []byte) (*Message, int, error) {
	if len(buf) < minMessageLen {
		return nil, 0, nil
	}
	totalLen := int(binary.BigEndian.Uint32(buf[0:4]))
	if len(buf) < totalLen {
		return nil, 0, nil
	}
	if totalLen < minMessageLen {
		return nil, 0, fmt.Errorf("frame total length %d below minimum %d", totalLen, minMessageLen)
	}
	frame := buf[:totalLen]
	headersLen := int(binary.BigEndian.Uint32(frame[4:8]))

	// prelude CRC
	preludeCRC := binary.BigEndian.Uint32(frame[8:12])
	if got := crc32.ChecksumIEEE(frame[0:8]); got != preludeCRC {
		return nil, 0, fmt.Errorf("prelude CRC mismatch: want %d got %d", preludeCRC, got)
	}
	// message CRC
	msgCRC := binary.BigEndian.Uint32(frame[totalLen-4:])
	if got := crc32.ChecksumIEEE(frame[:totalLen-4]); got != msgCRC {
		return nil, 0, fmt.Errorf("message CRC mismatch: want %d got %d", msgCRC, got)
	}

	headersStart := 12
	headersEnd := headersStart + headersLen
	if headersEnd > totalLen-4 {
		return nil, 0, fmt.Errorf("headers length %d overflows frame", headersLen)
	}

	headers := map[string]string{}
	off := headersStart
	for off < headersEnd {
		if off+1 > headersEnd {
			break
		}
		nameLen := int(frame[off])
		off++
		if off+nameLen > headersEnd {
			break
		}
		name := string(frame[off : off+nameLen])
		off += nameLen
		if off+1 > headersEnd {
			break
		}
		hType := frame[off]
		off++
		if hType != 7 {
			// Only string headers are needed; bail out of this header set.
			break
		}
		if off+2 > headersEnd {
			break
		}
		valLen := int(binary.BigEndian.Uint16(frame[off : off+2]))
		off += 2
		if off+valLen > headersEnd {
			break
		}
		headers[name] = string(frame[off : off+valLen])
		off += valLen
	}

	payload := append([]byte(nil), frame[headersEnd:totalLen-4]...)
	return &Message{Headers: headers, Payload: payload}, totalLen, nil
}
