package utils

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/cloudflare/cfssl/log"
	sentry "github.com/getsentry/sentry-go"
	"go.keploy.io/server/pkg/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

// askForConfirmation asks the user for confirmation. A user must type in "yes" or "no" and
// then press enter. It has fuzzy matching, so "y", "Y", "yes", "YES", and "Yes" all count as
// confirmations. If the input is not recognized, it will ask again. The function does not return
// until it gets a valid response from the user.
func AskForConfirmation(s string) (bool, error) {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Printf("%s [y/n]: ", s)

		response, err := reader.ReadString('\n')
		if err != nil {
			return false, err
		}

		response = strings.ToLower(strings.TrimSpace(response))

		if response == "y" || response == "yes" {
			return true, nil
		} else if response == "n" || response == "no" {
			return false, nil
		}
	}
}

func CheckFileExists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

var KeployVersion string

func attachLogFileToSentry(logFilePath string) {
	file, err := os.Open(logFilePath)
	if err != nil {
		errors.New(fmt.Sprintf("Error opening log file: %s", err.Error()))
		return
	}
	defer file.Close()

	content, _ := ioutil.ReadAll(file)

	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetExtra("logfile", string(content))
	})
	sentry.Flush(time.Second * 5)
}

func HandlePanic() {
	if r := recover(); r != nil {
		attachLogFileToSentry("./keploy-logs.txt")
		sentry.CaptureException(errors.New(fmt.Sprint(r)))
		log.Error(Emoji+"Recovered from:", r)
		sentry.Flush(time.Second * 2)
	}
}

func CheckMongoCollectionCount(mongoClient *mongo.Client, logger *zap.Logger) int {
	mocksFilter := bson.M{"name": bson.M{"$regex": models.TestSetMocks + ".*$"}}
	testFilter := bson.M{"name": bson.M{"$regex": models.TestSetTests + ".*$"}}

	mockCollections, err := mongoClient.Database(models.Keploy).ListCollectionNames(context.Background(), mocksFilter)
	if err != nil {
		logger.Error("unknown to fetch mock collection", zap.Error(err))
	}

	testCollections, err := mongoClient.Database(models.Keploy).ListCollectionNames(context.Background(), testFilter)
	if err != nil {
		logger.Error("unknown to fetch test collection", zap.Error(err))
	}

	collectionCount := len(mockCollections)
	if len(testCollections) > len(mockCollections) {
		collectionCount = len(testCollections)
	}
	return collectionCount
}

func ExtractAfterPattern(s, pattern string) string {
	parts := strings.Split(s, pattern)
	if len(parts) > 1 {
		return strings.TrimPrefix(parts[1], "-")
	}
	return ""
}
