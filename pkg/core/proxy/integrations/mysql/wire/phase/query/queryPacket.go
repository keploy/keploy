//go:build linux

// Package query provides functions to decode MySQL command phase packets.
package query

import (
	"context"
	"fmt"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

// COM_QUERY: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_query.html

func DecodeQuery(_ context.Context, data []byte) (*mysql.QueryPacket, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("query packet too short")
	}

	packet := &mysql.QueryPacket{
		Command: data[0],
		Query:   ReplaceTabsWithSpaces(string(data[1:])),
	}

	return packet, nil
}

// This is required to replace tabs with spaces in the query string, as yaml does not support tabs.
func ReplaceTabsWithSpaces(query string) string {
	result := ""
	for _, char := range query {
		if char == '\t' {
			result += "    " // 4 spaces
		} else {
			result += string(char)
		}
	}
	return result
}
