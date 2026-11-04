package kv

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// On-disk record layout, little-endian:
//
//	crc32 (4) | keyLen (4) | valLen (4) | kind (1) | key | value
//
// The checksum covers everything after itself, so a torn or corrupted
// record is detected on replay.
const headerSize = 4 + 4 + 4 + 1

const (
	kindPut    byte = 0
	kindDelete byte = 1
)

// Limits keep a corrupted length field from making recovery allocate gigabytes.
const (
	MaxKeySize   = 64 << 10
	MaxValueSize = 64 << 20
)

var (
	ErrKeyTooLarge   = errors.New("kv: key too large")
	ErrValueTooLarge = errors.New("kv: value too large")
	ErrEmptyKey      = errors.New("kv: empty key")
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

func encodeRecord(kind byte, key, value []byte) []byte {
	buf := make([]byte, headerSize+len(key)+len(value))
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(key)))
	binary.LittleEndian.PutUint32(buf[8:], uint32(len(value)))
	buf[12] = kind
	copy(buf[headerSize:], key)
	copy(buf[headerSize+len(key):], value)
	binary.LittleEndian.PutUint32(buf[0:], crc32.Checksum(buf[4:], crcTable))
	return buf
}

type header struct {
	crc    uint32
	keyLen uint32
	valLen uint32
	kind   byte
}

func decodeHeader(b []byte) header {
	return header{
		crc:    binary.LittleEndian.Uint32(b[0:]),
		keyLen: binary.LittleEndian.Uint32(b[4:]),
		valLen: binary.LittleEndian.Uint32(b[8:]),
		kind:   b[12],
	}
}

// plausible rejects headers no valid write could have produced.
func (h header) plausible() bool {
	return h.keyLen > 0 && h.keyLen <= MaxKeySize && h.valLen <= MaxValueSize &&
		(h.kind == kindPut || (h.kind == kindDelete && h.valLen == 0))
}

func checkKV(key, value []byte) error {
	switch {
	case len(key) == 0:
		return ErrEmptyKey
	case len(key) > MaxKeySize:
		return ErrKeyTooLarge
	case len(value) > MaxValueSize:
		return ErrValueTooLarge
	}
	return nil
}
