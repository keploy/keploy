package models

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlLib "gopkg.in/yaml.v3"
)

func TestHTTPResp_UnmarshalYAML_StreamingSSEBody(t *testing.T) {
	input := `
status_code: 200
header:
  Content-Type: text/event-stream
body:
  - ts: 2026-02-23T11:17:07.5708415Z
    data:
      comment: heartbeat
  - ts: 2026-02-23T11:17:07.57090625Z
    data:
      id: "1"
      event: message
      data: '{"ok":true}'
status_message: OK
proto_major: 1
proto_minor: 1
timestamp: 2026-02-23T11:17:07.57090625Z
`

	var got HTTPResp
	require.NoError(t, yamlLib.Unmarshal([]byte(input), &got))

	require.Len(t, got.StreamBody, 2)
	assert.Equal(t, "comment", got.StreamBody[0].Data[0].Key)
	assert.Equal(t, "heartbeat", got.StreamBody[0].Data[0].Value)
	assert.Equal(t, "id", got.StreamBody[1].Data[0].Key)
	assert.Equal(t, "event", got.StreamBody[1].Data[1].Key)
	assert.Equal(t, "data", got.StreamBody[1].Data[2].Key)
	assert.Equal(t, "1", got.StreamBody[1].Data[0].Value)
	assert.Contains(t, got.Body, ":heartbeat")
	assert.Contains(t, got.Body, "id:1")
	assert.Contains(t, got.Body, `data:{"ok":true}`)
}

func TestHTTPResp_UnmarshalYAML_StreamingRawBody(t *testing.T) {
	input := `
status_code: 200
header:
  Content-Type: application/x-ndjson
body:
  - ts: 2026-02-23T11:17:07.5708415Z
    data:
      raw: '{"chunk_id":1}'
  - ts: 2026-02-23T11:17:08.5708415Z
    data:
      raw: '{"chunk_id":2}'
status_message: OK
proto_major: 1
proto_minor: 1
timestamp: 2026-02-23T11:17:08.5708415Z
`

	var got HTTPResp
	require.NoError(t, yamlLib.Unmarshal([]byte(input), &got))

	require.Len(t, got.StreamBody, 2)
	assert.Equal(t, "raw", got.StreamBody[0].Data[0].Key)
	assert.Equal(t, `{"chunk_id":1}`, got.StreamBody[0].Data[0].Value)
	assert.Equal(t, `{"chunk_id":1}`+"\n"+`{"chunk_id":2}`, got.Body)
}

func TestHTTPResp_UnmarshalYAML_LegacyTextPlainAutoDerivesRawChunks(t *testing.T) {
	input := `
status_code: 200
header:
  Content-Type: text/plain
body: |
  line-1
  line-2
status_message: OK
proto_major: 1
proto_minor: 1
timestamp: 2026-02-23T11:17:08.5708415Z
`

	var got HTTPResp
	require.NoError(t, yamlLib.Unmarshal([]byte(input), &got))

	require.Len(t, got.StreamBody, 2)
	assert.Equal(t, "raw", got.StreamBody[0].Data[0].Key)
	assert.Equal(t, "line-1", got.StreamBody[0].Data[0].Value)
	assert.Equal(t, "line-2", got.StreamBody[1].Data[0].Value)
	assert.Contains(t, got.Body, "line-1")
	assert.Contains(t, got.Body, "line-2")
}

func TestHTTPResp_MarshalYAML_StreamingSSEBody(t *testing.T) {
	ts := time.Date(2026, 2, 23, 11, 17, 7, 570906250, time.UTC)
	resp := HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"Content-Type": "text/event-stream",
		},
		Body:      "id:1\nevent:message\ndata:{\"ok\":true}\n\n",
		Timestamp: ts,
	}

	out, err := yamlLib.Marshal(resp)
	require.NoError(t, err)
	body := string(out)

	assert.Contains(t, body, "\nbody:\n")
	assert.Contains(t, body, "- ts:")
	assert.Contains(t, body, "data:")
	assert.Contains(t, body, "id: \"1\"")
	assert.Contains(t, body, "event: message")
	assert.Contains(t, body, "data: '{\"ok\":true}'")
	assert.False(t, strings.Contains(body, "body: |"), "streaming body should not be serialized as scalar block")
}

func TestHTTPResp_MarshalYAML_TextPlainStreamingBodyAsRawChunks(t *testing.T) {
	resp := HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"Content-Type": "text/plain",
		},
		Body:      "line-1\nline-2\nline-3\n",
		Timestamp: time.Date(2026, 2, 24, 5, 53, 37, 0, time.UTC),
	}

	out, err := yamlLib.Marshal(resp)
	require.NoError(t, err)
	body := string(out)

	assert.Contains(t, body, "\nbody:\n")
	assert.Contains(t, body, "raw: line-1")
	assert.Contains(t, body, "raw: line-2")
	assert.Contains(t, body, "raw: line-3")
	assert.False(t, strings.Contains(body, "body: |"), "text/plain stream body should be serialized as chunk list")
}

func TestHTTPResp_MarshalYAML_SSEMultilineDataUsesSingleDataField(t *testing.T) {
	resp := HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"Content-Type": "text/event-stream",
		},
		Body: strings.Join([]string{
			"id:1",
			"event:message",
			"data:line-1",
			"data:line-2",
			"",
		}, "\n"),
		Timestamp: time.Date(2026, 2, 24, 5, 53, 37, 0, time.UTC),
	}

	out, err := yamlLib.Marshal(resp)
	require.NoError(t, err)
	body := string(out)

	// Avoid duplicate "data:" keys in a YAML map by storing multiline data in one scalar.
	assert.Contains(t, body, "data: |-")
	assert.Contains(t, body, "line-1")
	assert.Contains(t, body, "line-2")
}
