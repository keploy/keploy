package redisv2

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"

	"go.keploy.io/server/v2/pkg/models"
)

type RespValue interface{}

type parser struct {
	data []byte
	pos  int
}

// NewParser creates a parser for the given string
func NewParser(s string) *parser {
	return &parser{data: []byte(s)}
}

// ParseEntry parses a single RESP3 value
func (p *parser) ParseEntry() (RespValue, error) {
	if p.pos >= len(p.data) {
		return nil, errors.New("no more data")
	}

	switch p.data[p.pos] {
	case '*':
		return p.parseArray()
	case '%':
		return p.parseMap()
	case '$':
		return p.parseBulkString()
	case ':':
		return p.parseInteger()
	case '+':
		return p.parseSimpleString()
	default:
		return nil, fmt.Errorf("unexpected prefix '%c' at pos %d", p.data[p.pos], p.pos)
	}
}

// readLine reads until the next "\r\n" (not including it) and advances pos past it.
func (p *parser) readLine() ([]byte, error) {
	idx := bytes.Index(p.data[p.pos:], []byte("\r\n"))
	if idx < 0 {
		return nil, errors.New("CRLF not found")
	}
	line := p.data[p.pos : p.pos+idx]
	p.pos += idx + 2
	return line, nil
}

func (p *parser) parseInteger() (RespValue, error) {
	p.pos++ // skip ':'
	line, err := p.readLine()
	if err != nil {
		return nil, err
	}
	i, err := strconv.ParseInt(string(line), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid integer %q: %w", line, err)
	}
	return i, nil
}

func (p *parser) parseBulkString() (RespValue, error) {
	p.pos++ // skip '$'
	line, err := p.readLine()
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(string(line))
	if err != nil {
		return nil, fmt.Errorf("invalid bulk length %q: %w", line, err)
	}
	if n < 0 {
		return nil, nil // nil bulk string
	}
	if p.pos+n+2 > len(p.data) {
		return nil, errors.New("bulk string length exceeds data")
	}
	str := string(p.data[p.pos : p.pos+n])
	p.pos += n + 2 // skip string + CRLF
	return str, nil
}

func (p *parser) parseSimpleString() (RespValue, error) {
	p.pos++ // skip '+'
	line, err := p.readLine()
	if err != nil {
		return nil, err
	}
	return string(line), nil
}

func (p *parser) parseArray() (RespValue, error) {
	p.pos++ // skip '*'
	line, err := p.readLine()
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(string(line))
	if err != nil {
		return nil, fmt.Errorf("invalid array size %q: %w", line, err)
	}
	arr := make([]interface{}, count)
	for i := 0; i < count; i++ {
		elem, err := p.ParseEntry()
		if err != nil {
			return nil, err
		}
		arr[i] = elem
	}
	return arr, nil
}

func (p *parser) parseMap() (RespValue, error) {
	p.pos++ // skip '%'
	line, err := p.readLine()
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(string(line))
	if err != nil {
		return nil, fmt.Errorf("invalid map size %q: %w", line, err)
	}
	m := make(map[string]interface{}, count)
	for i := 0; i < count; i++ {
		keyRaw, err := p.ParseEntry()
		if err != nil {
			return nil, fmt.Errorf("reading map key: %w", err)
		}
		key, ok := keyRaw.(string)
		if !ok {
			return nil, fmt.Errorf("map key is not a string: %T", keyRaw)
		}
		val, err := p.ParseEntry()
		if err != nil {
			return nil, fmt.Errorf("reading map value for key %q: %w", key, err)
		}
		m[key] = val
	}
	return m, nil
}

// ParseAll reads entries until data is exhausted.
func (p *parser) ParseAll() ([]RespValue, error) {
	var vals []RespValue
	for p.pos < len(p.data) {
		// skip stray CRLFs
		if p.data[p.pos] == '\r' || p.data[p.pos] == '\n' {
			p.pos++
			continue
		}
		v, err := p.ParseEntry()
		if err != nil {
			return nil, err
		}
		vals = append(vals, v)
	}
	return vals, nil
}

func parseRedis(buf []byte) ([]models.RedisBodyType, error) {
	prs := NewParser(string(buf))
	vals, err := prs.ParseAll()
	if err != nil {
		return nil, err
	}
	bodies := make([]models.RedisBodyType, len(vals))
	for i, v := range vals {
		b, err := toRedisBody(v)
		if err != nil {
			return nil, err
		}
		bodies[i] = b
	}
	return bodies, nil
}

func toRedisBody(v RespValue) (models.RedisBodyType, error) {
	switch t := v.(type) {
	case []interface{}:
		elems := make([]models.RedisBodyType, len(t))
		for i, e := range t {
			b, err := toRedisBody(e)
			if err != nil {
				return models.RedisBodyType{}, err
			}
			elems[i] = b
		}
		return models.RedisBodyType{Type: "array", Size: len(elems), Data: elems}, nil

	case map[string]interface{}:
		entries := make([]models.RedisMapBody, 0, len(t))
		for k, v2 := range t {
			keyElem := models.RedisElement{Length: len(k), Value: k}
			valElem := models.RedisElement{}
			switch v3 := v2.(type) {
			case string:
				valElem = models.RedisElement{Length: len(v3), Value: v3}
			case int64:
				s := strconv.FormatInt(v3, 10)
				valElem = models.RedisElement{Length: len(s), Value: v3}
			default:
				nested, err := toRedisBody(v3)
				if err != nil {
					return models.RedisBodyType{}, err
				}
				valElem = models.RedisElement{Length: 0, Value: nested}
			}
			entries = append(entries, models.RedisMapBody{Key: keyElem, Value: valElem})
		}
		return models.RedisBodyType{Type: "map", Size: len(entries), Data: entries}, nil

	case string:
		return models.RedisBodyType{Type: "string", Size: len(t), Data: t}, nil

	case int64:
		s := strconv.FormatInt(t, 10)
		return models.RedisBodyType{Type: "integer", Size: len(s), Data: t}, nil

	default:
		return models.RedisBodyType{}, fmt.Errorf("unsupported type %T", v)
	}
}
