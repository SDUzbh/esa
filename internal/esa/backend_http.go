package esa

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var errStateCAS = errors.New("authoritative state revision conflict")

type HTTPStateBackend struct {
	URL, Token string
	Client     *http.Client
}
type statePacket struct {
	State    State  `json:"state"`
	Revision uint64 `json:"revision"`
}

func (b HTTPStateBackend) request(method string, input any, known ...uint64) (statePacket, error) {
	var result statePacket
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return result, err
		}
		body = bytes.NewReader(data)
	}
	r, err := http.NewRequest(method, b.URL, body)
	if err != nil {
		return result, err
	}
	r.Header.Set("Authorization", "Bearer "+b.Token)
	r.Header.Set("Content-Type", "application/json")
	if len(known) > 0 {
		r.Header.Set("If-None-Match", strconv.Quote(strconv.FormatUint(known[0], 10)))
	}
	client := b.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return fmt.Errorf("state redirects are not permitted") }
	client = &copyClient
	resp, err := client.Do(r)
	if err != nil {
		return result, fmt.Errorf("authoritative state transport unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && len(known) > 0 {
		return statePacket{Revision: known[0]}, nil
	}
	if resp.StatusCode == 409 {
		return result, errStateCAS
	}
	if resp.StatusCode != 200 {
		return result, fmt.Errorf("authoritative state returned HTTP %d", resp.StatusCode)
	}
	d := json.NewDecoder(io.LimitReader(resp.Body, 64<<20))
	if err = d.Decode(&result); err != nil {
		return result, fmt.Errorf("invalid authoritative state response")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return result, fmt.Errorf("unexpected authoritative state response")
	}
	return result, nil
}
func (b HTTPStateBackend) Load() (State, uint64, error) {
	packet, err := b.request(http.MethodGet, nil)
	return packet.State, packet.Revision, err
}
func (b HTTPStateBackend) LoadIfChanged(known uint64) (State, uint64, bool, error) {
	packet, err := b.request(http.MethodGet, nil, known)
	if err == nil && packet.State.SchemaVersion != 0 {
		err = validateState(packet.State)
	}
	return packet.State, packet.Revision, packet.Revision != known, err
}
func (b HTTPStateBackend) Commit(state State, revision uint64) (uint64, error) {
	packet, err := b.request(http.MethodPut, map[string]any{"expected_revision": revision, "state": state})
	return packet.Revision, err
}
func (b HTTPStateBackend) Validate() error {
	u, err := url.Parse(b.URL)
	if err != nil || u.User != nil || u.Host == "" || len(b.Token) < 24 || !(strings.HasPrefix(b.URL, "http://") || strings.HasPrefix(b.URL, "https://")) {
		return fmt.Errorf("valid state URL and private service token required")
	}
	return nil
}
