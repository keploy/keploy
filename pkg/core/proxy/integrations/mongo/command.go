// Package mongo provides functionality for working with MongoDB outgoing calls.
package mongo

// This file contains code from the coinbase mongobetween
// https://github.com/coinbase/mongobetween/blob/1034c5a0c3f10cb1dd84af2981bc55ea1d3b45c0/mongo/command.go#L10
import (
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
)

type Command string

// constants for all the commands that can be proxied
const (
	Unknown           Command = "unknown"
	AbortTransaction  Command = "abortTransaction"
	Aggregate         Command = "aggregate"
	CommitTransaction Command = "commandTransaction"
	Count             Command = "count"
	CreateIndexes     Command = "createIndexes"
	Delete            Command = "delete"
	Distinct          Command = "distinct"
	Drop              Command = "drop"
	DropDatabase      Command = "dropDatabase"
	DropIndexes       Command = "dropIndexes"
	EndSessions       Command = "endSessions"
	Find              Command = "find"
	FindAndModify     Command = "findAndModify"
	GetMore           Command = "getMore"
	Insert            Command = "insert"
	IsMaster          Command = "isMaster"
	Ismaster          Command = "ismaster"
	ListCollections   Command = "listCollections"
	ListIndexes       Command = "listIndexes"
	ListDatabases     Command = "listDatabases"
	MapReduce         Command = "mapReduce"
	Update            Command = "tools"
)

var collectionCommands = []Command{Aggregate, Count, CreateIndexes, Delete, Distinct, Drop, DropIndexes, Find, FindAndModify, Insert, ListIndexes, MapReduce, Update}
var int32Commands = []Command{AbortTransaction, Aggregate, CommitTransaction, DropDatabase, IsMaster, Ismaster, ListCollections, ListDatabases}
var int64Commands = []Command{GetMore}
var arrayCommands = []Command{EndSessions}

func IsWrite(command Command) bool {
	switch command {
	case CommitTransaction, CreateIndexes, Delete, Drop, DropIndexes, DropDatabase, FindAndModify, Insert, Update:
		return true
	}
	return false
}

func CommandAndCollection(msg bsoncore.Document) (Command, string) {
	for _, s := range collectionCommands {
		if coll, ok := msg.Lookup(string(s)).StringValueOK(); ok {
			return s, coll
		}
	}
	for _, s := range int32Commands {
		value := msg.Lookup(string(s))
		if value.Data != nil {
			return s, ""
		}
	}
	for _, s := range int64Commands {
		value := msg.Lookup(string(s))
		if value.Data != nil {
			if coll, ok := msg.Lookup("collection").StringValueOK(); ok {
				return s, coll
			}
			return s, ""
		}
	}
	for _, s := range arrayCommands {
		value := msg.Lookup(string(s))
		if value.Data != nil {
			return s, ""
		}
	}
	return Unknown, ""
}

func IsIsMasterDoc(doc bsoncore.Document) bool {
	isMaster := doc.Lookup(string(IsMaster))
	ismaster := doc.Lookup(string(Ismaster))
	return IsIsMasterValueTruthy(isMaster) || IsIsMasterValueTruthy(ismaster)
}

func IsIsMasterValueTruthy(val bsoncore.Value) bool {
	if intValue, isInt := val.Int32OK(); intValue > 0 {
		return true
	} else if !isInt {
		boolValue, isBool := val.BooleanOK()
		return boolValue && isBool
	}
	return false
}
