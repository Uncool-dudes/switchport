// Package dns answers A queries for custom TLDs with the loopback address.
package dns

import (
	"encoding/binary"
	"net"
	"strings"
)

const (
	headerLen   = 12
	maxLabelLen = 63
	maxPacket   = 512
	typeA       = 1
	rcodeNX     = 3
	ttlSeconds  = 5
)

// Serve answers on pc until it is closed.
func Serve(pc net.PacketConn, tlds []string) {
	buf := make([]byte, maxPacket)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if reply, ok := Answer(buf[:n], tlds); ok {
			// UDP is best effort, and the client retries.
			_, _ = pc.WriteTo(reply, addr)
		}
	}
}

// Answer builds the reply to one query. It returns false for anything it
// cannot parse, and answers NXDOMAIN outside tlds, so it can never be used to
// resolve real names.
func Answer(q []byte, tlds []string) ([]byte, bool) {
	if len(q) < headerLen || binary.BigEndian.Uint16(q[4:]) != 1 {
		return nil, false
	}
	var labels []string
	i := headerLen
	for {
		if i >= len(q) {
			return nil, false
		}
		n := int(q[i])
		if n == 0 {
			i++
			break
		}
		if n > maxLabelLen || i+1+n > len(q) {
			return nil, false
		}
		labels = append(labels, string(q[i+1:i+1+n]))
		i += 1 + n
	}
	if i+4 > len(q) {
		return nil, false
	}
	qtype := binary.BigEndian.Uint16(q[i:])
	end := i + 4
	name := strings.ToLower(strings.Join(labels, "."))

	owned := false
	for _, tld := range tlds {
		owned = owned || strings.HasSuffix(name, "."+tld)
	}
	reply := append([]byte(nil), q[:end]...)
	reply[2] = 0x84 | (q[2] & 0x01) // response, authoritative, copy RD
	reply[3] = 0
	binary.BigEndian.PutUint16(reply[6:], 0)
	binary.BigEndian.PutUint16(reply[8:], 0)
	binary.BigEndian.PutUint16(reply[10:], 0)
	if !owned {
		reply[3] = rcodeNX
		return reply, true
	}
	if qtype != typeA {
		return reply, true
	}
	binary.BigEndian.PutUint16(reply[6:], 1)
	// 0xC00C is a compression pointer to the question's name.
	reply = append(reply, 0xC0, 0x0C, 0, typeA, 0, 1, 0, 0, 0, ttlSeconds, 0, 4, 127, 0, 0, 1)
	return reply, true
}
