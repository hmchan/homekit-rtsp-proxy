package hap

import (
	"bytes"
	"fmt"
	"io"
)

// TLV8 implements the Type-Length-Value encoding used by the HomeKit Accessory Protocol.
// Values longer than 255 bytes are split across consecutive TLV items with the same type.

type TLV8Item struct {
	Type  byte
	Value []byte
}

func TLV8Encode(items []TLV8Item) []byte {
	var buf bytes.Buffer
	for _, item := range items {
		val := item.Value
		for len(val) > 255 {
			buf.WriteByte(item.Type)
			buf.WriteByte(255)
			buf.Write(val[:255])
			val = val[255:]
		}
		buf.WriteByte(item.Type)
		buf.WriteByte(byte(len(val)))
		buf.Write(val)
	}
	return buf.Bytes()
}

func TLV8Decode(data []byte) ([]TLV8Item, error) {
	var items []TLV8Item
	r := bytes.NewReader(data)

	for r.Len() > 0 {
		typ, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("tlv8: read type: %w", err)
		}
		length, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("tlv8: read length: %w", err)
		}
		val := make([]byte, length)
		if length > 0 {
			if _, err := io.ReadFull(r, val); err != nil {
				return nil, fmt.Errorf("tlv8: read value: %w", err)
			}
		}

		// Merge consecutive items with the same type (fragmented values).
		if len(items) > 0 && items[len(items)-1].Type == typ && length == 255 {
			items[len(items)-1].Value = append(items[len(items)-1].Value, val...)
		} else if len(items) > 0 && items[len(items)-1].Type == typ && len(items[len(items)-1].Value)%255 == 0 {
			// Previous fragment was exactly 255, this is the last fragment.
			items[len(items)-1].Value = append(items[len(items)-1].Value, val...)
		} else {
			items = append(items, TLV8Item{Type: typ, Value: val})
		}
	}

	return items, nil
}

// TLV8GetBytes returns the value for the first item with the given type.
func TLV8GetBytes(items []TLV8Item, typ byte) []byte {
	for _, item := range items {
		if item.Type == typ {
			return item.Value
		}
	}
	return nil
}

// TLV8GetByte returns the first byte of the value for the given type.
func TLV8GetByte(items []TLV8Item, typ byte) (byte, bool) {
	v := TLV8GetBytes(items, typ)
	if len(v) == 0 {
		return 0, false
	}
	return v[0], true
}
