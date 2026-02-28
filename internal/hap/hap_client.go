package hap

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HAPClient sends encrypted HTTP requests over a verified HAP connection.
// It handles both request/response and asynchronous EVENT notifications
// on a single encrypted connection using a reader goroutine.
type HAPClient struct {
	enc    *EncryptedConn
	reader *bufio.Reader
	addr   string

	// Request serialization: only one request at a time.
	reqMu   sync.Mutex
	// Response channel: the reader goroutine sends buffered responses here.
	respCh  chan *readResult
	// Event channel: asynchronous EVENT notifications.
	eventCh chan *CharacteristicsResponse
	// Done signal for the reader goroutine.
	done    chan struct{}
}

// bufferedResponse holds a fully-read HTTP response.
type bufferedResponse struct {
	StatusCode int
	Body       []byte
}

type readResult struct {
	resp *bufferedResponse
	err  error
}

// NewHAPClient creates a client for encrypted HAP communication.
func NewHAPClient(enc *EncryptedConn) *HAPClient {
	c := &HAPClient{
		enc:     enc,
		reader:  bufio.NewReaderSize(enc, 4096),
		addr:    enc.RemoteAddr().String(),
		respCh:  make(chan *readResult, 1),
		eventCh: make(chan *CharacteristicsResponse, 16),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// readLoop continuously reads from the encrypted connection and dispatches
// HTTP responses and EVENT notifications to the appropriate channels.
func (c *HAPClient) readLoop() {
	defer close(c.done)
	for {
		// Peek to check if it's an EVENT or HTTP response.
		peek, err := c.reader.Peek(5)
		if err != nil {
			c.respCh <- &readResult{err: err}
			return
		}

		isEvent := string(peek) == "EVENT"
		if isEvent {
			// Read the full "EVENT/1.0" prefix and swap it.
			full, err := c.reader.Peek(9)
			if err != nil {
				c.respCh <- &readResult{err: err}
				return
			}
			copy(full, "HTTP/1.1 ")
		}

		resp, err := http.ReadResponse(c.reader, nil)
		if err != nil {
			if isEvent {
				continue
			}
			c.respCh <- &readResult{err: fmt.Errorf("read HTTP response: %w", err)}
			return
		}

		// Fully read the body before proceeding so the reader is clean
		// for the next message.
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			if isEvent {
				continue
			}
			c.respCh <- &readResult{err: fmt.Errorf("read body: %w", err)}
			return
		}

		if isEvent {
			var result CharacteristicsResponse
			if err := json.Unmarshal(body, &result); err != nil {
				continue
			}
			select {
			case c.eventCh <- &result:
			default:
			}
		} else {
			c.respCh <- &readResult{
				resp: &bufferedResponse{
					StatusCode: resp.StatusCode,
					Body:       body,
				},
			}
		}
	}
}

// Accessory represents a HAP accessory with its services.
type Accessory struct {
	AID      int       `json:"aid"`
	Services []Service `json:"services"`
}

// Service represents a HAP service with its characteristics.
type Service struct {
	IID             int              `json:"iid"`
	Type            string           `json:"type"`
	Characteristics []Characteristic `json:"characteristics"`
}

// Characteristic represents a HAP characteristic.
type Characteristic struct {
	AID    int         `json:"aid,omitempty"`
	IID    int         `json:"iid"`
	Type   string      `json:"type,omitempty"`
	Value  interface{} `json:"value,omitempty"`
	Perms  []string    `json:"perms,omitempty"`
	Format string      `json:"format,omitempty"`
	Ev     *bool       `json:"ev,omitempty"`
}

// AccessoriesResponse is the JSON response from GET /accessories.
type AccessoriesResponse struct {
	Accessories []Accessory `json:"accessories"`
}

// CharacteristicsRequest is the JSON body for PUT /characteristics.
type CharacteristicsRequest struct {
	Characteristics []Characteristic `json:"characteristics"`
}

// CharacteristicsResponse is the JSON response from GET /characteristics.
type CharacteristicsResponse struct {
	Characteristics []Characteristic `json:"characteristics"`
}

// GetAccessories fetches the accessory database.
func (c *HAPClient) GetAccessories() (*AccessoriesResponse, error) {
	resp, err := c.doRequest("GET", "/accessories", nil)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(resp.Body))
	}

	var result AccessoriesResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("parse accessories: %w", err)
	}
	return &result, nil
}

// GetCharacteristics reads characteristic values.
func (c *HAPClient) GetCharacteristics(ids ...string) (*CharacteristicsResponse, error) {
	path := "/characteristics?id=" + strings.Join(ids, ",")
	resp, err := c.doRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(resp.Body))
	}

	var result CharacteristicsResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("parse characteristics: %w", err)
	}
	return &result, nil
}

// PutCharacteristics writes characteristic values.
func (c *HAPClient) PutCharacteristics(chars []Characteristic) error {
	req := CharacteristicsRequest{Characteristics: chars}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	resp, err := c.doRequest("PUT", "/characteristics", body)
	if err != nil {
		return err
	}

	// 204 No Content = success with no body.
	// 200 OK = success with body.
	// 207 Multi-Status = partial success; check individual statuses.
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusMultiStatus {
		// Check if any characteristic returned a non-zero status.
		var result struct {
			Characteristics []struct {
				AID    int `json:"aid"`
				IID    int `json:"iid"`
				Status int `json:"status"`
			} `json:"characteristics"`
		}
		if err := json.Unmarshal(resp.Body, &result); err == nil {
			for _, ch := range result.Characteristics {
				if ch.Status != 0 {
					return fmt.Errorf("characteristic %d.%d error status %d", ch.AID, ch.IID, ch.Status)
				}
			}
		}
		return nil
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(resp.Body))
}

// SubscribeCharacteristic enables event notifications for a characteristic.
func (c *HAPClient) SubscribeCharacteristic(aid, iid int) error {
	ev := true
	return c.PutCharacteristics([]Characteristic{
		{AID: aid, IID: iid, Ev: &ev},
	})
}

// Events returns the channel for receiving asynchronous EVENT notifications.
func (c *HAPClient) Events() <-chan *CharacteristicsResponse {
	return c.eventCh
}

// doRequest sends an HTTP request and waits for the buffered response.
// Only one request can be in-flight at a time.
func (c *HAPClient) doRequest(method, path string, body []byte) (*bufferedResponse, error) {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()

	var reqBuf bytes.Buffer
	fmt.Fprintf(&reqBuf, "%s %s HTTP/1.1\r\n", method, path)
	fmt.Fprintf(&reqBuf, "Host: %s\r\n", c.addr)

	if body != nil {
		fmt.Fprintf(&reqBuf, "Content-Type: application/hap+json\r\n")
		fmt.Fprintf(&reqBuf, "Content-Length: %d\r\n", len(body))
	}
	fmt.Fprintf(&reqBuf, "\r\n")
	if body != nil {
		reqBuf.Write(body)
	}

	if _, err := c.enc.Write(reqBuf.Bytes()); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	select {
	case result := <-c.respCh:
		if result.err != nil {
			return nil, result.err
		}
		return result.resp, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("HAP request timeout after 30s")
	case <-c.done:
		return nil, fmt.Errorf("HAP connection closed")
	}
}

// Close closes the encrypted connection.
func (c *HAPClient) Close() error {
	return c.enc.Close()
}
