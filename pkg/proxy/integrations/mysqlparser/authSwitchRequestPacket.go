package mysqlparser

import (
	"errors"
)

type AuthSwitchRequest struct {
	PluginName string `yaml:"plugin_name"`
	Data       []byte `yaml:"data"`
}

func decodeAuthSwitchRequest(data []byte) (*AuthSwitchRequest, error) {
	if len(data) < 2 {
		return nil, errors.New("invalid auth switch request packet")
	}

	pluginName, _, err := nullTerminatedString(data[1:])
	if err != nil {
		return nil, err
	}

	authSwitchData := data[len(pluginName)+2:]

	return &AuthSwitchRequest{
		PluginName: pluginName,
		Data:       authSwitchData,
	}, nil
}
