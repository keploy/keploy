// Package main is the entry point for the keploy application.
package main

import (
	"context"
	"fmt"
	"os"
	"syscall"

	"go.keploy.io/server/v2/cli"
	"go.keploy.io/server/v2/cli/provider"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb"
	"go.keploy.io/server/v2/utils"
	"go.keploy.io/server/v2/utils/log"
	//pprof for debugging
	// _ "net/http/pprof"
)

// version is the version of the server and will be injected during build by ldflags, same with dsn
// see https://goreleaser.com/customization/build/

var version string
var dsn string

const logo string = `
       ▓██▓▄
    ▓▓▓▓██▓█▓▄
     ████████▓▒
          ▀▓▓███▄      ▄▄   ▄               ▌
         ▄▌▌▓▓████▄    ██ ▓█▀  ▄▌▀▄  ▓▓▌▄   ▓█  ▄▌▓▓▌▄ ▌▌   ▓
       ▓█████████▌▓▓   ██▓█▄  ▓█▄▓▓ ▐█▌  ██ ▓█  █▌  ██  █▌ █▓
      ▓▓▓▓▀▀▀▀▓▓▓▓▓▓▌  ██  █▓  ▓▌▄▄ ▐█▓▄▓█▀ █▓█ ▀█▄▄█▀   █▓█
       ▓▌                           ▐█▌                   █▌
        ▓
`

func main() {

	// Uncomment the following code to enable pprof for debugging
	// go func() {
	// 	fmt.Println("Starting pprof server for debugging...")
	// 	http.ListenAndServe("localhost:6060", nil)
	// }()
	printLogo()
	ctx := utils.NewCtx()
	start(ctx)
}

func printLogo() {
	if version == "" {
		version = "2-dev"
	}
	utils.Version = version
	if binaryToDocker := os.Getenv("BINARY_TO_DOCKER"); binaryToDocker != "true" {
		fmt.Println(logo, " ")
		fmt.Printf("version: %v\n\n", version)
	}
}

func start(ctx context.Context) {
	logger, err := log.New()
	if err != nil {
		fmt.Println("Failed to start the logger for the CLI", err)
		return
	}
	defer utils.DeleteLogs(logger)
	defer utils.Recover(logger)

	// The 'umask' command is commonly used in various operating systems to regulate the permissions of newly created files.
	// These 'umask' values subtract from the permissions assigned by the process, effectively lowering the permissions.
	// For example, if a file is created with permissions '777' and the 'umask' is '022', the resulting permissions will be '755',
	// reducing certain permissions for security purposes.
	// Setting 'umask' to '0' ensures that 'keploy' can precisely control the permissions of the files it creates.
	// However, it's important to note that this approach may not work in scenarios involving mounted volumes,
	// as the 'umask' is set by the host system, and cannot be overridden by 'keploy' or individual processes.
	oldMask := syscall.Umask(0)
	defer syscall.Umask(oldMask)

	configDb := configdb.NewConfigDb(logger)
	if dsn != "" {
		utils.SentryInit(logger, dsn)
		//logger = utils.ModifyToSentryLogger(ctx, logger, sentry.CurrentHub().Client(), configDb)
	}
	conf := config.New()
	svcProvider := provider.NewServiceProvider(logger, configDb, conf)
	cmdConfigurator := provider.NewCmdConfigurator(logger, conf)
	rootCmd := cli.Root(ctx, logger, svcProvider, cmdConfigurator)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// // pprof for debugging
// // _ "net/http/pprof"
// func main() {
// 	mxSim := 0.9
// 	// TODO: need find a proper similarity index to set a benchmark for matching or need to find another way to do approximate matching
// 	mxIdx := -1
// 	str1 := "*12\r\n$7\r\nevalsha\r\n$40\r\n8b912cdc5b4c20108ef73d952464fba3a7470d7b\r\n$1\r\n6\r\n$27\r\nbull:aggregateCheck:delayed\r\n$26\r\nbull:aggregateCheck:active\r\n$24\r\nbull:aggregateCheck:wait\r\n$28\r\nbull:aggregateCheck:priority\r\n$26\r\nbull:aggregateCheck:paused\r\n$31\r\nbull:aggregateCheck:meta-paused\r\n$20\r\nbull:aggregateCheck:\r\n$13\r\n1716557524267\r\n$36\r\nc27ef38b-df9b-4e54-8765-38babda4d0fc\r\n"
// 	str2 := "*12\r\n$7\r\nevalsha\r\n$40\r\n8b912cdc5b4c20108ef73d952464fba3a7470d7b\r\n$1\r\n6\r\n$27\r\nbull:aggregateCheck:delayed\r\n$26\r\nbull:aggregateCheck:active\r\n$24\r\nbull:aggregateCheck:wait\r\n$28\r\nbull:aggregateCheck:priority\r\n$26\r\nbull:aggregateCheck:paused\r\n$31\r\nbull:aggregateCheck:meta-paused\r\n$20\r\nbull:aggregateCheck:\r\n$13\r\n1716616639810\r\n$36\r\n4ec296ac-5419-48f6-a115-c10ea154c73e\r\n"

// 	similarity := fuzzyCheck([]byte(str1), []byte(str2))
// 	fmt.Println(similarity)

// 	if mxSim < similarity {
// 		mxSim = similarity
// 	}

// 	if mxIdx == -1 {
// 		// fmt.Println("there is difference bro")
// 	}
// 	fmt.Println(mxSim)
// }

// func fuzzyCheck(encoded, reqBuf []byte) float64 {
// 	k := util.AdaptiveK(len(reqBuf), 3, 8, 5)
// 	shingles1 := util.CreateShingles(encoded, k)
// 	shingles2 := util.CreateShingles(reqBuf, k)
// 	similarity := util.JaccardSimilarity(shingles1, shingles2)
// 	return similarity
// }
