package hap

import (
	"bytes"
	"testing"
)

func TestTLV8RoundTrip(t *testing.T) {
	items := []TLV8Item{
		{Type: 0x01, Value: []byte{0xAA, 0xBB}},
		{Type: 0x02, Value: []byte{0xCC}},
		{Type: 0x03, Value: []byte{}},
	}

	encoded := TLV8Encode(items)
	decoded, err := TLV8Decode(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(decoded) != len(items) {
		t.Fatalf("expected %d items, got %d", len(items), len(decoded))
	}

	for i, item := range items {
		if decoded[i].Type != item.Type {
			t.Errorf("item %d: type %02x != %02x", i, decoded[i].Type, item.Type)
		}
		if !bytes.Equal(decoded[i].Value, item.Value) {
			t.Errorf("item %d: value mismatch", i)
		}
	}
}

func TestTLV8LongValue(t *testing.T) {
	// Value longer than 255 bytes should be fragmented.
	longVal := make([]byte, 300)
	for i := range longVal {
		longVal[i] = byte(i % 256)
	}

	items := []TLV8Item{{Type: 0x05, Value: longVal}}
	encoded := TLV8Encode(items)
	decoded, err := TLV8Decode(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(decoded) != 1 {
		t.Fatalf("expected 1 item, got %d", len(decoded))
	}
	if !bytes.Equal(decoded[0].Value, longVal) {
		t.Errorf("long value mismatch: got %d bytes, want %d", len(decoded[0].Value), len(longVal))
	}
}

func TestTLV8GetBytes(t *testing.T) {
	items := []TLV8Item{
		{Type: 0x01, Value: []byte{0x10}},
		{Type: 0x02, Value: []byte{0x20, 0x21}},
	}

	if v := TLV8GetBytes(items, 0x02); !bytes.Equal(v, []byte{0x20, 0x21}) {
		t.Errorf("unexpected value: %v", v)
	}
	if v := TLV8GetBytes(items, 0x99); v != nil {
		t.Errorf("expected nil for missing type, got %v", v)
	}
}
