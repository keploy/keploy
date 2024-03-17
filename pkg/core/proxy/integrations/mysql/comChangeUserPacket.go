package mysql

import (
	"encoding/base64"
	"errors"
	"strings"
)

type ComChangeUserPacket struct {
	User         string `json:"user,omitempty" yaml:"user,omitempty,flow"`
	Auth         string `json:"auth,omitempty" yaml:"auth,omitempty,flow"`
	Db           string `json:"db,omitempty" yaml:"db,omitempty,flow"`
	CharacterSet uint8  `json:"character_set,omitempty" yaml:"character_set,omitempty,flow"`
	AuthPlugin   string `json:"auth_plugin,omitempty" yaml:"auth_plugin,omitempty,flow"`
}

func decodeComChangeUser(data []byte) (ComChangeUserPacket, error) {
	if len(data) < 2 {
		return ComChangeUserPacket{}, errors.New("Data too short for COM_CHANGE_USER")
	}

	nullTerminatedStrings := strings.Split(string(data[1:]), "\x00")
	if len(nullTerminatedStrings) < 5 {
		return ComChangeUserPacket{}, errors.New("Data malformed for COM_CHANGE_USER")
	}

	user := nullTerminatedStrings[0]
	authLength := data[len(user)+2]
	auth := data[len(user)+3 : len(user)+3+int(authLength)]
	db := nullTerminatedStrings[2]
	characterSet := data[len(user)+4+int(authLength)]
	authPlugin := nullTerminatedStrings[3]

	return ComChangeUserPacket{
		User:         user,
		Auth:         base64.StdEncoding.EncodeToString(auth),
		Db:           db,
		CharacterSet: characterSet,
		AuthPlugin:   authPlugin,
	}, nil
}
